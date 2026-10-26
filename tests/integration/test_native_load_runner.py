"""OFFLINE supervisor guards. All process/network/SQL calls below are fakes.

These tests never start a process, signal a PID, connect to a native service,
grant real delegation, or create retained live acceptance evidence.
"""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import native_load_runner as n
import fault_acceptance as f
import stress_acceptance as stress
from deployed_api_harness import Config, DriverError

FID = "11111111-1111-4111-8111-111111111111"
UID = "22222222-2222-4222-8222-222222222222"


def offline_fault_report():
    report = f.new_report(list(n.FAULT_SCENARIOS))
    report["execution"] = "executed"
    for case in report["cases"]:
        if case["scenario"] in n.FAULT_SCENARIOS:
            case["status"] = "pass"  # OFFLINE orchestration fixture, never live evidence.
    f.summarize(report, list(n.FAULT_SCENARIOS))
    return report


class OwnedProcessGuards(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.service = n.OwnedService("worker", ["/OFFLINE/fixture-worker"], {}, Path(self.tmp.name) / "worker.log")

    def tearDown(self):
        self.tmp.cleanup()

    def test_start_uses_argv_without_shell_and_does_not_duplicate(self):
        process = Mock()
        process.poll.return_value = None
        with patch.object(n.subprocess, "Popen", return_value=process) as popen:
            self.service.start()
            self.service.start()
        popen.assert_called_once()
        self.assertEqual(popen.call_args.args[0], ["/OFFLINE/fixture-worker"])
        self.assertNotIn("shell", popen.call_args.kwargs)
        self.assertTrue(popen.call_args.kwargs["close_fds"])

    def test_stop_only_terminates_its_owned_handle(self):
        process = Mock()
        process.poll.return_value = None
        self.service.process = process
        self.service.stop()
        process.terminate.assert_called_once_with()
        process.wait.assert_called_once_with(timeout=2)
        process.kill.assert_not_called()

    def test_timeout_does_not_escalate_to_sigkill(self):
        process = Mock()
        process.poll.return_value = None
        process.wait.side_effect = subprocess.TimeoutExpired("OFFLINE", 2)
        self.service.process = process
        with self.assertRaisesRegex(DriverError, "owned_service_graceful_stop_timeout"):
            self.service.stop()
        process.kill.assert_not_called()

    def test_no_handle_means_no_signal_or_external_pid_adoption(self):
        with patch.object(n.subprocess, "Popen") as popen:
            self.service.stop()
        popen.assert_not_called()
        self.assertFalse(self.service.running())

    def test_fault_controls_exclude_api_maintenance_and_postgres(self):
        services = {name: Mock() for name in ("worker", "nats", "meili", "api", "maintenance", "postgres")}
        controls = n.owned_fault_controls(services)
        self.assertEqual(set(controls), {"worker", "nats", "meili"})
        f.Hooks(controls).validate()


class SnapshotGuards(unittest.TestCase):
    def test_snapshot_is_scoped_readonly_bounded_and_has_attempt_proof(self):
        snapshot = n.Snapshot("/OFFLINE/psql", {"PGHOST": "bad", "PGOPTIONS": "bad",
                            "LD_LIBRARY_PATH": "/OFFLINE/libs"}, 55432)
        with patch.object(n.subprocess, "run", return_value=SimpleNamespace(returncode=0, stdout="{}")) as run:
            self.assertEqual(snapshot(FID, UID), {})
        self.assertEqual(run.call_args.kwargs["timeout"], 2)
        env = run.call_args.kwargs["env"]
        self.assertNotIn("PGHOST", env)
        self.assertIn("default_transaction_read_only=on", env["PGOPTIONS"])
        self.assertIn("statement_timeout=1000", env["PGOPTIONS"])
        self.assertEqual(env["LD_LIBRARY_PATH"], "/OFFLINE/libs")
        argv = run.call_args.args[0]
        self.assertIn("-XAt", argv)
        sql = argv[-1]
        self.assertIn("max_unpublished_outbox_attempt", sql)
        self.assertIn("COALESCE(max(attempt),0) FROM unpublished", sql)
        self.assertIn("count(*) FROM unpublished", sql)
        self.assertIn("file_id='" + FID + "'", sql)
        self.assertIn("upload_id='" + UID + "'", sql)
        self.assertNotIn("UPDATE ", sql)
        self.assertNotIn("INSERT ", sql)
        self.assertNotIn("DELETE ", sql)

    def test_bad_uuid_never_reaches_sql_process(self):
        with patch.object(n.subprocess, "run") as run, self.assertRaises(DriverError):
            n.Snapshot("/OFFLINE/psql", {}, 55432)("'; DROP TABLE files;--", UID)
        run.assert_not_called()

    def test_failed_sql_response_is_not_reflected(self):
        with patch.object(n.subprocess, "run", return_value=SimpleNamespace(returncode=1, stdout="SECRET")), \
                self.assertRaisesRegex(DriverError, "readonly_snapshot_failed"):
            n.Snapshot("/OFFLINE/psql", {}, 55432)(FID, UID)


class PrivateConfigGuards(unittest.TestCase):
    def test_retention_namespace_is_unique_safe_and_not_the_prior_default(self):
        with patch.object(n.secrets, "token_hex", side_effect=["a" * 16, "b" * 16]) as token:
            first, second = n.new_retention_namespace(), n.new_retention_namespace()
        self.assertNotEqual(first, second)
        self.assertRegex(first, r"^\.clouddrive-retained-native-[a-f0-9]{16}$")
        self.assertRegex(second, r"^\.clouddrive-retained-native-[a-f0-9]{16}$")
        self.assertNotEqual(first, ".clouddrive-retained")
        self.assertEqual([call.args for call in token.call_args_list], [(8,), (8,)])

    def test_nats_config_is_private_exact_loopback_and_bounded(self):
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp)
            path = n.write_nats_config(run_dir)
            self.assertEqual(path.parent, run_dir)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            content = path.read_text()
            self.assertIn('listen: "127.0.0.1:54222"', content)
            self.assertIn('http: "127.0.0.1:58222"', content)
            self.assertIn('max_memory_store: 256MB', content)
            self.assertIn('max_file_store: 1GB', content)
            self.assertIn(json.dumps(str(run_dir / "jetstream")), content)
            self.assertNotIn("0.0.0.0", content)
            second = n.write_nats_config(run_dir)
            self.assertNotEqual(path, second)

    def test_existing_config_is_not_overwritten_or_followed(self):
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp)
            existing = run_dir / "nats-load-fixed.conf"
            existing.write_text("preserve first failure configuration")
            with patch.object(n.secrets, "token_hex", return_value="fixed"), self.assertRaises(FileExistsError):
                n.write_nats_config(run_dir)
            self.assertEqual(existing.read_text(), "preserve first failure configuration")


class ScopeAndConsentGuards(unittest.TestCase):
    def test_changed_source_cannot_keep_selected_or_overall_pass(self):
        report = {"status": "pass", "selected_status": "pass", "source_sha256_at_launch": "a" * 64}
        self.assertFalse(n.confirm_source_stability(report, "b" * 64))
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["selected_status"], "fail")
        self.assertIs(report["source_changed_during_run"], True)
        self.assertEqual(report["reason"], "source_changed_during_run")

    def test_unknown_initial_or_final_hash_cannot_keep_pass(self):
        for initial, final in (("a" * 64, None), (None, "a" * 64), (None, None), ("", ""), ("invalid", "invalid")):
            report = {"status": "partial", "selected_status": "pass", "source_sha256_at_launch": initial}
            self.assertFalse(n.confirm_source_stability(report, final))
            self.assertEqual(report["status"], "fail")
            self.assertEqual(report["selected_status"], "fail")
            self.assertEqual(report["source_changed_during_run"], "unknown")
            self.assertEqual(report["reason"], "source_changed_during_run")

    def test_equal_known_source_preserves_scope_and_prior_failure(self):
        for status in ("pass", "partial", "fail"):
            report = {"status": status, "source_sha256_at_launch": "a" * 64}
            self.assertTrue(n.confirm_source_stability(report, "a" * 64))
            self.assertEqual(report["status"], status)
            self.assertIs(report["source_changed_during_run"], False)

    def test_exact_four_pass_partial_report_is_accepted_without_full_pass(self):
        report = n.accept_selected_fault_result(3, offline_fault_report())
        self.assertEqual(report["selected_status"], "pass")
        self.assertEqual(report["full_fault_acceptance"], "partial")
        self.assertEqual(report["native_move_response_loss"], "not_run")
        self.assertEqual(report["selected_scenarios"], list(n.FAULT_SCENARIOS))

    def test_missing_duplicate_extra_or_unrun_case_rejected(self):
        for mutation in (lambda r: r["cases"].pop(0), lambda r: r["cases"].append(r["cases"][0]),
                         lambda r: r["cases"][0].update(status="not_run"),
                         lambda r: r["cases"][0].update(scenario="unknown")):
            report = offline_fault_report()
            mutation(report)
            with self.assertRaises(DriverError):
                n.accept_selected_fault_result(3, report)

    def test_fake_full_pass_and_wrong_exit_rejected(self):
        for code, changes in ((0, {}), (3, {"status": "pass"}), (3, {"full_fault_acceptance": "pass"}),
                              (3, {"selected_status": "partial"}), (3, {"execution": "not_run"})):
            report = {**offline_fault_report(), **changes}
            with self.subTest(code=code, changes=changes), self.assertRaises(DriverError):
                n.accept_selected_fault_result(code, report)

    def test_standalone_stress_requires_optin_before_login(self):
        with patch.object(n, "API") as api, self.assertRaises(DriverError):
            n.standalone_stress_preflight(Mock(), {})
        api.assert_not_called()

    def test_standalone_stress_logs_in_then_grants_explicit_fixture_delegation(self):
        with patch.object(n, "API") as api:
            n.standalone_stress_preflight(Mock(), {"ALLOW_DISPOSABLE_TEST_OIDC_CONSENT": "yes"})
        self.assertEqual(api.return_value.method_calls,
                         [unittest.mock.call.login(), unittest.mock.call.delegate(allow_new_grant=True)])

    def test_stress_cli_includes_explicit_write_optin(self):
        args = n.stress_arguments(Path("/OFFLINE/runtime.json"), Path("/OFFLINE/report.json"))
        self.assertEqual(args, ["--runtime-file", "/OFFLINE/runtime.json", "--output", "/OFFLINE/report.json",
                                "--allow-disposable-writes", "--max-concurrency", "16", "--max-seconds", "600"])

    def test_only_explicit_bounded_concurrency_choices_are_forwarded(self):
        for maximum in (16, 32, 64):
            args = n.stress_arguments("/OFFLINE/runtime.json", "/OFFLINE/report.json", maximum)
            self.assertEqual(args[-4:-2], ["--max-concurrency", str(maximum)])
            self.assertEqual(args[-2:], ["--max-seconds", "600"])
            self.assertNotIn("--payload-bytes", args)
        for maximum in (0, 1, 128, True, "64", 64.0):
            with self.subTest(maximum=maximum), self.assertRaises(DriverError):
                n.stress_arguments("/OFFLINE/runtime.json", "/OFFLINE/report.json", maximum)

    def test_cli_rejects_unbounded_concurrency_before_starting_services(self):
        with patch.object(n, "OwnedService") as service, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit) as caught:
            n.main(["--mode", "all", "--output-dir", "/OFFLINE", "--max-concurrency", "128"])
        self.assertEqual(caught.exception.code, 2)
        service.assert_not_called()

    def test_pressure_time_budget_is_bounded_not_expanded(self):
        self.assertEqual(n.stress_arguments("/OFFLINE/runtime", "/OFFLINE/output", 64, 350)[-2:],
                         ["--max-seconds", "350"])
        for seconds in (0, 59, 601, True, "600", 600.0):
            with self.assertRaises(DriverError):
                n.stress_arguments("/OFFLINE/runtime", "/OFFLINE/output", 64, seconds)


class SupervisorOrchestrationGuards(unittest.TestCase):
    def test_fault_supervisor_keeps_partial_and_builds_valid_runtime_contract(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            runtime, run_dir, output = root / "runtime", root / "runtime/run", root / "report"
            (runtime / "postgres/usr/lib/x86_64-linux-gnu").mkdir(parents=True)
            (run_dir / "pgdata").mkdir(parents=True)
            (run_dir / "pgdata/postmaster.pid").write_text("5001\n")
            for rel in ("nats-server-v2.15.1-linux-amd64/nats-server", "meilisearch", "bin/drive-api",
                        "bin/drive-worker", "bin/drive-maintenance"):
                binary = runtime / rel
                binary.parent.mkdir(parents=True, exist_ok=True)
                binary.write_bytes(b"OFFLINE NOT EXECUTABLE SOFTWARE")
                binary.chmod(0o700)
            config = Config("http://127.0.0.1:58081", "https://localhost:58443/realms/clouddrive",
                            {"owner": {"username": "OFFLINE_OWNER", "password": "OFFLINE_SECRET"}})
            env = {"ALLOW_DISPOSABLE_TEST_WRITES": "yes", "ALLOW_DISPOSABLE_TEST_LOAD": "yes",
                   "ALLOW_DISPOSABLE_TEST_FAULTS": "yes", "ALLOW_DISPOSABLE_TEST_OIDC_CONSENT": "yes",
                   "NATIVE_RUNTIME": str(runtime), "NATIVE_RUN_DIR": str(run_dir),
                   "NATIVE_KEYCLOAK_PID": "5002", "NATIVE_OCIS_PID": "5003"}
            services = []
            app_environments = {}
            def fake_service(*args):
                service = Mock()
                service.process = SimpleNamespace(pid=5100 + len(services))
                service.running.return_value = True
                services.append(service)
                if args[0] in ("api", "worker", "maintenance"):
                    app_environments[args[0]] = args[2]
                return service
            with patch.dict(os.environ, env, clear=True), patch.object(n.Config, "from_environment", return_value=config), \
                    patch.object(n, "OwnedService", side_effect=fake_service), patch.object(n, "wait_health"), \
                    patch.object(n.threading, "Timer", return_value=Mock(is_alive=Mock(return_value=False))), \
                    patch.object(n, "source_hash", return_value="a" * 64), \
                    patch.object(n.subprocess, "run", return_value=SimpleNamespace(returncode=0)), \
                    patch.object(f, "run", return_value=(3, offline_fault_report())) as fault_run, \
                    contextlib.redirect_stdout(io.StringIO()):
                rc = n.main(["--mode", "fault", "--output-dir", str(output)])
            self.assertEqual(rc, 3)
            report = json.loads((output / "supervisor.json").read_text())
            self.assertEqual(report["status"], "partial")
            self.assertEqual(set(app_environments), {"api", "worker", "maintenance"})
            reserved = {e["DRIVE_TRASH_RESERVED_DIR"] for e in app_environments.values()}
            self.assertEqual(len(reserved), 1)
            self.assertRegex(next(iter(reserved)), r"^\.clouddrive-retained-native-[a-f0-9]{16}$")
            self.assertIs(app_environments["api"], app_environments["worker"])
            self.assertIs(app_environments["api"], app_environments["maintenance"])
            self.assertEqual(report["fixture_isolation"]["native_retention_namespace"], "unique_per_supervisor_run")
            self.assertNotIn(next(iter(reserved)), json.dumps(report))
            self.assertEqual(report["bounded_resources"]["configured_stress_maximum_concurrency"], 16)
            self.assertEqual(report["selected_status"], "pass")
            self.assertEqual(report["scenarios"]["fault"]["full_fault_acceptance"], "partial")
            manifests = list(run_dir.glob("load-runtime-*.json"))
            self.assertEqual(len(manifests), 1)
            self.assertFalse((output / "runtime.json").exists())
            self.assertFalse(list(output.glob("*.conf")))
            spec = json.loads(manifests[0].read_text())
            self.assertIs(spec["exclusive_owner"], True)
            self.assertEqual(spec["postgres"]["library_path"], str(runtime / "postgres/usr/lib/x86_64-linux-gnu"))
            self.assertEqual(manifests[0].stat().st_mode & 0o777, 0o600)
            self.assertEqual(report["source_sha256_at_launch"], "a" * 64)
            self.assertFalse(report["source_changed_during_run"])
            self.assertEqual(fault_run.call_args.kwargs["scenarios"], list(n.FAULT_SCENARIOS))
            self.assertEqual(set(fault_run.call_args.args[2].services), {"worker", "nats", "meili"})
            self.assertTrue(all(s.stop.called for s in services))


class OverallDeadlineGuards(unittest.TestCase):
    def test_watchdog_only_stops_owned_services_and_is_idempotent(self):
        services = {name: Mock() for name in n.OWNED_SERVICES}
        with patch.object(n.time, "monotonic", return_value=100), patch.object(n.threading, "Timer") as timer:
            budget = n.RunDeadline(100)
            budget.arm(services)
            timer.assert_called_once_with(570, budget.expire)
            budget.expire()
            budget.expire()
            for service in services.values():
                service.stop.assert_called_once_with()
                with self.assertRaisesRegex(DriverError, "overall_run_deadline_reached"):
                    service.start_guard()

    def test_external_native_dependencies_cannot_join_stop_targets(self):
        with patch.object(n.threading, "Timer") as timer, self.assertRaisesRegex(DriverError, "deadline_service_not_owned"):
            n.RunDeadline(0).arm({"postgres": Mock()})
        timer.assert_not_called()

    def test_expiration_blocks_actual_owned_service_start_before_popen(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(n.time, "monotonic", return_value=0), \
                patch.object(n.threading, "Timer"), patch.object(n.subprocess, "Popen") as popen:
            budget = n.RunDeadline(0)
            service = n.OwnedService("worker", ["/OFFLINE/worker"], {}, Path(tmp) / "worker.log")
            budget.arm({"worker": service})
            budget.expire()
            with self.assertRaises(DriverError):
                service.start()
        popen.assert_not_called()

    def test_shutdown_failure_and_expired_budget_cannot_pass(self):
        service = Mock()
        service.stop.side_effect = ValueError("PRIVATE failure")
        with patch.object(n.time, "monotonic", return_value=0), patch.object(n.threading, "Timer"):
            budget = n.RunDeadline(0)
            budget.arm({"worker": service})
            budget.expire()
            report = {"status": "pass", "selected_status": "pass"}
            self.assertFalse(budget.finish(report))
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["selected_status"], "fail")
        self.assertEqual(report["reason"], "overall_run_deadline_reached")
        self.assertEqual(report["whole_run_budget"]["owned_stop_failures"], ["worker"])
        self.assertNotIn("PRIVATE", json.dumps(report))

    def test_late_return_is_failure_even_if_watchdog_did_not_fire(self):
        with patch.object(n.time, "monotonic", return_value=601):
            report = {"status": "partial", "selected_status": "pass"}
            self.assertFalse(n.RunDeadline(0).finish(report))
        self.assertEqual(report["status"], "fail")
        self.assertFalse(report["whole_run_budget"]["watchdog_fired"])

    def test_pressure_budget_reserves_cleanup_and_fault_window(self):
        with patch.object(n.time, "monotonic", return_value=100):
            budget = n.RunDeadline(0)
            self.assertEqual(budget.pressure_seconds(reserve_faults=True), 350)
            self.assertEqual(budget.pressure_seconds(reserve_faults=False), 470)
        with patch.object(n.time, "monotonic", return_value=400), self.assertRaisesRegex(DriverError, "insufficient_overall_budget"):
            budget.pressure_seconds(reserve_faults=True)

    def test_unexpired_budget_preserves_prior_scope_and_failure(self):
        with patch.object(n.time, "monotonic", return_value=500):
            for status in ("pass", "partial", "fail"):
                report = {"status": status}
                self.assertTrue(n.RunDeadline(0).finish(report))
                self.assertEqual(report["status"], status)

    def test_finish_joins_started_watchdog_and_rejects_lingering_callback(self):
        with patch.object(n.time, "monotonic", return_value=500):
            budget = n.RunDeadline(0)
            budget.timer = Mock()
            budget.timer.is_alive.return_value = True
            report = {"status": "pass", "selected_status": "pass"}
            self.assertFalse(budget.finish(report))
        budget.timer.cancel.assert_called_once()
        budget.timer.join.assert_called_once_with(timeout=12)
        self.assertTrue(report["whole_run_budget"]["watchdog_shutdown_incomplete"])
        self.assertEqual(report["selected_status"], "fail")

    def test_finish_at_work_cutoff_fails_even_before_timer_callback_sets_flag(self):
        with patch.object(n.time, "monotonic", return_value=570):
            budget = n.RunDeadline(0)
            budget.timer = Mock(is_alive=Mock(return_value=False))
            report = {"status": "partial", "selected_status": "pass"}
            self.assertFalse(budget.finish(report))
        self.assertEqual(report["status"], "fail")
        self.assertFalse(report["whole_run_budget"]["watchdog_fired"])


class BaselineDrainGuards(unittest.TestCase):
    @staticmethod
    def sample(backlog=0):
        return {"conversion_pending": 0, "index_pending": 0, "outbox_pending": backlog,
                "sampled_rss_bytes": 1024, "available_ram_bytes": 1 << 32, "free_disk_bytes": 1 << 40,
                "active_connections": 1, "waiting_locks": 0, "metrics_complete": True,
                "native_rss_scope": "configured_pids", "rss_by_pid": {"SECRET_PID": 1024},
                "process_identities": {"SECRET_PID": {"start_ticks": "PRIVATE"}}}

    def test_three_consecutive_zero_samples_one_second_apart_with_safety(self):
        clock = [0.0]
        result = {}
        samples = [self.sample(n) for n in (0, 0, 5, 0, 0, 0)]
        with patch.object(stress.Safety, "sample", side_effect=samples) as sample, \
                patch.object(n.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(n.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)) as sleep:
            n.wait_baseline_drain({"native_pids": {"fixture": 2}}, result)
        self.assertEqual(sample.call_count, 6)
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [1] * 5)
        self.assertEqual(result["status"], "pass")
        self.assertEqual(result["consecutive_zero_samples"], 3)
        self.assertEqual(result["stable_zero_span_seconds"], 2)
        self.assertEqual(result["elapsed_seconds"], 5)
        self.assertNotIn("SECRET_PID", json.dumps(result))
        self.assertNotIn("PRIVATE", json.dumps(result))

    def test_resource_guard_failure_stops_transition(self):
        result = {}
        with patch.object(stress.Safety, "sample", side_effect=DriverError("sampled_rss_limit_reached")) as sample, \
                patch.object(n.time, "sleep") as sleep, self.assertRaises(DriverError):
            n.wait_baseline_drain({}, result)
        sample.assert_called_once()
        sleep.assert_not_called()
        self.assertEqual(result["status"], "fail")

    def test_missing_native_rss_is_not_treated_as_zero(self):
        result = {}
        with patch.object(stress.Safety, "sample", return_value={**self.sample(), "native_rss_scope": "not_checked"}), \
                self.assertRaisesRegex(DriverError, "baseline_native_resource_metrics_incomplete"):
            n.wait_baseline_drain({}, result)
        self.assertEqual(result["status"], "fail")

    def test_never_drained_pipeline_times_out_without_load(self):
        clock, result = [0.0], {}
        with patch.object(stress.Safety, "sample", side_effect=lambda: self.sample(1)), \
                patch.object(n.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(n.time, "sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)), \
                self.assertRaisesRegex(DriverError, "baseline_pipeline_drain_timeout"):
            n.wait_baseline_drain({}, result)
        self.assertEqual(result["status"], "fail")
        self.assertLessEqual(result["elapsed_seconds"], 120)


if __name__ == "__main__":
    unittest.main()
