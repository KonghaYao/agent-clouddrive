"""Offline guard/accounting tests. These are NOT deployed stress-run evidence."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import sys
import subprocess
import select
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from deployed_api_harness import Config, DriverError, Response, Transport
from stress_acceptance import (GIB, MAX_BYTES, MAX_WORK, Metrics, Options, PG_FIELDS, PG_SQL,
    Safety, Session, Suite, WORKLOADS, literal_loopback, main, new_report, percentile,
    private_json, run, safe_reason, summarize, threshold_reason, validate_runtime,
    parse_proc_stat, process_snapshot, select_process_trees, sample_process_trees, counter_threshold, summarize_resource_window)


def row(seconds=1, error=False, conflict=False):
    return {"seconds": seconds, "unexpected": error, "expected_409": conflict}


def resource(**extra):
    return {"available_ram_bytes": 3 * GIB, "free_disk_bytes": 6 * GIB,
            "sampled_rss_bytes": GIB, **{k: 0 for k in PG_FIELDS}, **extra}


class StatisticsTests(unittest.TestCase):
    def test_nearest_rank_small_and_large(self):
        self.assertEqual(percentile([5, 1, 3, 2, 4], .5), 3)
        self.assertEqual(percentile(range(1, 101), .95), 95)
        self.assertEqual(percentile(range(1, 101), .99), 99)
        self.assertEqual(percentile([7], .99), 7)

    def test_empty_is_unknown_not_zero(self):
        result = summarize([], 10)
        self.assertEqual(result["count"], 0)
        for field in ("p50_seconds", "p95_seconds", "p99_seconds", "unexpected_error_ratio", "expected_409_ratio"):
            self.assertIsNone(result[field])

    def test_expected409_separate_denominator(self):
        result = summarize([row(conflict=True), row(conflict=True), row(error=True), row()], 2)
        self.assertEqual(result["unexpected_error_ratio"], .25)
        self.assertEqual(result["expected_409"], 2)
        self.assertEqual(result["throughput_per_second"], 2)

    def test_exact_ten_percent_not_above_threshold(self):
        self.assertIsNone(threshold_reason(summarize([row(error=True)] + [row()] * 9, 1)))
        self.assertEqual(threshold_reason(summarize([row(error=True)] + [row()] * 8, 1)),
                         "unexpected_error_ratio_exceeded")

    def test_latency_strict_threshold(self):
        self.assertIsNone(threshold_reason(summarize([row(10)], 1)))
        self.assertEqual(threshold_reason(summarize([row(10.001)], 1)), "p99_latency_exceeded")

    def test_functional_failure_can_stop_even_successful_http(self):
        self.assertEqual(threshold_reason(summarize([row()], 1), summarize([row(error=True)], 1)),
                         "unexpected_error_ratio_exceeded")

    def test_integer_guard_p99_boundaries_1_99_100_101(self):
        for count, tolerated in ((1, 0), (99, 0), (100, 1), (101, 1)):
            self.assertIsNone(counter_threshold(count, 0, tolerated))
            self.assertEqual(counter_threshold(count, 0, tolerated + 1), "p99_latency_exceeded")
            values = [row(10)] * (count - tolerated) + [row(10.001)] * tolerated
            self.assertEqual(counter_threshold(count, 0, tolerated), threshold_reason(summarize(values, 1)))

    def test_integer_error_boundaries_and_expected_conflicts(self):
        for count in (1, 99, 100, 101, 199, 200):
            allowed = count // 10
            self.assertIsNone(counter_threshold(count, allowed, 0))
            self.assertEqual(counter_threshold(count, allowed + 1, 0), "unexpected_error_ratio_exceeded")
        metrics = Metrics()
        for _ in range(101):
            metrics.record("http", 1, "same_file_conflict", 10, expected409=True, status=409)
        self.assertIsNone(metrics.guard_reason(1))
        self.assertEqual(metrics.guard_counters[("http", 1, "same_file_conflict")], [101, 0, 0])

    def test_integer_guards_equal_sorted_reference_without_dropping_samples(self):
        for count in (1, 2, 10, 99, 100, 101, 199, 200):
            for slow in range(count + 1):
                values = [row(1)] * (count - slow) + [row(10.1)] * slow
                self.assertEqual(counter_threshold(count, 0, slow), threshold_reason(summarize(values, 1)))
        metrics = Metrics()
        for index in range(101):
            metrics.record("http", 2, "download", 11 if index == 0 else 1, status=200)
        self.assertEqual(len(metrics.http), 101)
        self.assertEqual(metrics.summary("http", 2, "download")["p99_seconds"], 1)
        self.assertIsNone(metrics.guard_reason(2))

    def test_incremental_stage_and_workload_scope_and_no_hot_path_sort(self):
        metrics = Metrics()
        for _ in range(20):
            metrics.record("http", 1, "list", 1, status=200)
        metrics.record("http", 1, "download", 1, unexpected=True, status=500)
        self.assertEqual(metrics.guard_reason(1), "unexpected_error_ratio_exceeded")  # Workload, not stage ratio.
        self.assertIsNone(metrics.guard_reason(2))
        safety = Safety({}, Options(), metrics)
        with patch.object(metrics, "summary", side_effect=AssertionError("guard must not scan/sort full samples")):
            safety.check_results(1)
        self.assertEqual(safety.reason, "unexpected_error_ratio_exceeded")

    def test_http_and_scenario_counters_remain_separate(self):
        metrics = Metrics()
        metrics.record("http", 1, "upload", 1, status=200)
        metrics.record("scenarios", 1, "upload", 10.001)
        self.assertEqual(metrics.guard_counters[("http", 1, "upload")], [1, 0, 0])
        self.assertEqual(metrics.guard_counters[("scenarios", 1, "upload")], [1, 0, 1])
        self.assertEqual(metrics.guard_reason(1), "p99_latency_exceeded")
        metrics.record("http", 2, "setup", 10, unexpected=True)
        self.assertEqual(metrics.guard_counters[("http", 2, None)], [1, 1, 0])
        self.assertEqual(metrics.guard_reason(2), "unexpected_error_ratio_exceeded")

    def test_per_workload_phase_accounting(self):
        metrics = Metrics()
        metrics.record("http", 1, "same_file_conflict", .1, expected409=True, status=409, phase="race")
        metrics.record("http", 1, "same_file_conflict", .2, status=200, phase="timed")
        self.assertEqual(metrics.summary("http", 1, "same_file_conflict", phase="timed")["expected_409"], 0)
        self.assertEqual(metrics.summary("http", 1, "same_file_conflict")["expected_409"], 1)
        self.assertEqual(metrics.summary("http", 2)["count"], 0)


class StopTests(unittest.TestCase):
    def test_resource_boundaries(self):
        self.assertIsNone(threshold_reason(sample=resource(available_ram_bytes=2*GIB, free_disk_bytes=5*GIB,
                                                          sampled_rss_bytes=4*GIB)))
        for changes, reason in [({"available_ram_bytes": 2*GIB-1}, "available_ram_below_2gib"),
                                ({"free_disk_bytes": 5*GIB-1}, "free_disk_below_5gib"),
                                ({"sampled_rss_bytes": 4*GIB+1}, "sampled_rss_exceeded_4gib"),
                                ({"conversion_pending": 5001}, "queued_work_exceeded_5000")]:
            with self.subTest(reason=reason):
                self.assertEqual(threshold_reason(sample=resource(**changes)), reason)

    def test_missing_memory_not_interpreted_as_low_memory(self):
        sample = resource()
        del sample["available_ram_bytes"]
        with self.assertRaisesRegex(DriverError, "required_resource_metrics_missing"):
            threshold_reason(sample=sample)

    def test_missing_pg_not_pass(self):
        sample = resource()
        del sample["index_pending"]
        with self.assertRaisesRegex(DriverError, "required_postgres_metrics_missing"):
            threshold_reason(sample=sample)

    def test_optional_rss_not_fabricated(self):
        self.assertIsNone(threshold_reason(sample=resource(sampled_rss_bytes=None)))

    def test_caps_reserve_before_write_and_retain_first_reason(self):
        safety = Safety({}, Options(max_new_bytes=65536), Metrics())
        safety.reserve(size=1024, work=1)
        self.assertEqual(safety.fixture_allocation_reserved, 65536)
        with self.assertRaisesRegex(DriverError, "fixture_byte_cap_reached"):
            safety.reserve(size=1)
        self.assertEqual(safety.bytes_reserved, 1024)
        safety.abort("first_failure")
        safety.abort("second_failure")
        with self.assertRaisesRegex(DriverError, "first_failure"):
            safety.guard()

    def test_maxwork_and_deadline(self):
        safety = Safety({}, Options(max_work_items=1), Metrics())
        safety.reserve(work=1)
        with self.assertRaisesRegex(DriverError, "work_item_cap_reached"):
            safety.reserve(work=1)
        safety.deadline = time.monotonic() - 1
        with self.assertRaisesRegex(DriverError, "total_time_limit_reached"):
            safety.guard()

    def test_concurrency_choices_are_fixed_bounded_prefixes(self):
        self.assertEqual(Options().schedule, (1, 4, 8, 16))
        self.assertEqual(Options(maximum_concurrency=32).schedule, (1, 4, 8, 16, 32))
        self.assertEqual(Options(maximum_concurrency=64).schedule, (1, 4, 8, 16, 32, 64))
        for cap in (0, 8, 17, 128, 16.0, True, "64"):
            with self.subTest(cap=cap), self.assertRaisesRegex(DriverError, "maximum_concurrency"):
                Options(maximum_concurrency=cap).validate()

    def test_extension_does_not_change_any_other_cap(self):
        baseline = Options()
        for maximum in (32, 64):
            options = Options(maximum_concurrency=maximum)
            self.assertEqual(options.max_new_bytes, baseline.max_new_bytes)
            self.assertEqual(options.max_work_items, baseline.max_work_items)
            self.assertEqual(options.max_seconds, baseline.max_seconds)
            self.assertEqual(options.stage_seconds, baseline.stage_seconds)
            self.assertEqual(threshold_reason(sample=resource(sampled_rss_bytes=4*GIB+1)), "sampled_rss_exceeded_4gib")

    def test_options_cannot_expand_hard_limits(self):
        for args in ({"stage_seconds": 31}, {"stage_seconds": float("nan")}, {"max_seconds": 601},
                     {"max_new_bytes": MAX_BYTES+1}, {"max_work_items": MAX_WORK+1}, {"payload_bytes": 65537}):
            with self.subTest(args=args), self.assertRaises(DriverError):
                Options(**args).validate()


class ResourceWindowTests(unittest.TestCase):
    def sample(self, elapsed, **changes):
        return {**resource(), "elapsed_seconds": elapsed, "metrics_complete": True,
                "native_rss_scope": "configured_pids", **changes}

    def test_inclusive_window_bounds_and_sampled_extrema(self):
        samples = [self.sample(.9, sampled_rss_bytes=99*GIB), self.sample(1, conversion_pending=4,
                    available_ram_bytes=3*GIB, free_disk_bytes=7*GIB),
                   self.sample(2, sampled_rss_bytes=2*GIB, index_pending=5, outbox_pending=2,
                    active_connections=6, waiting_locks=1, available_ram_bytes=2*GIB, free_disk_bytes=6*GIB),
                   self.sample(2.1, conversion_pending=999)]
        result = summarize_resource_window(samples, 1, 2)
        self.assertEqual(result["sample_count"], 2)
        self.assertEqual(result["peak_sampled_rss_bytes"], 2*GIB)
        self.assertEqual(result["peak_conversion_pending"], 4)
        self.assertEqual(result["peak_index_pending"], 5)
        self.assertEqual(result["peak_outbox_pending"], 2)
        self.assertEqual(result["peak_active_connections"], 6)
        self.assertEqual(result["peak_waiting_locks"], 1)
        self.assertEqual(result["peak_total_pending_work"], 7)  # Not the sum of independent peaks.
        self.assertEqual(result["min_available_ram_bytes"], 2*GIB)
        self.assertEqual(result["min_free_disk_bytes"], 6*GIB)
        self.assertEqual(result["sampling_basis"], "existing_point_in_time_observations_not_continuous")
        self.assertEqual(len(samples), 4)  # Full raw observations are untouched.

    def test_missing_samples_are_null_not_zero(self):
        result = summarize_resource_window([self.sample(1)], 2, 3)
        self.assertEqual(result["status"], "not_observed")
        self.assertEqual(result["sample_count"], 0)
        self.assertIsNone(result["peak_sampled_rss_bytes"])
        self.assertIsNone(result["peak_conversion_pending"])
        self.assertIsNone(result["min_free_disk_bytes"])

    def test_invalid_window_or_incomplete_selected_metrics_fail_closed(self):
        for window in ((2, 1), (-1, 1), (0, float("nan"))):
            with self.assertRaisesRegex(DriverError, "invalid_resource_window"):
                summarize_resource_window([], *window)
        for changes in ({"metrics_complete": False}, {"sampled_rss_bytes": None}, {"index_pending": -1}):
            with self.assertRaisesRegex(DriverError, "incomplete_stage_resource_sample"):
                summarize_resource_window([self.sample(1, **changes)], 0, 2)



class ProcessTreeTests(unittest.TestCase):
    @staticmethod
    def record(pid, ppid, rss=1024, state="S", started="1", name="fixture"):
        return {"pid": pid, "ppid": ppid, "rss_bytes": rss, "state": state,
                "start_ticks": started, "process_name": name}

    def test_parse_real_field_positions_and_parentheses(self):
        fields = ["S", "7"] + ["0"] * 17 + ["1234", "99999", "8"]
        parsed = parse_proc_stat("10 (a name (nested)) " + " ".join(fields), 10, 4096)
        self.assertEqual(parsed["ppid"], 7)
        self.assertEqual(parsed["start_ticks"], "1234")
        self.assertEqual(parsed["rss_bytes"], 8 * 4096)
        self.assertEqual(parsed["process_name"], "process")

    def test_descendant_transitive_closure_and_overlapping_roots(self):
        snapshot = {1: self.record(1, 0), 2: self.record(2, 1), 3: self.record(3, 2),
                    4: self.record(4, 1), 9: self.record(9, 0)}
        self.assertEqual(select_process_trees(snapshot, {"outer": 1, "inner": 2}),
                         {"outer": {1, 2, 3, 4}, "inner": {2, 3}})

    def test_rss_dedup_and_reparented_descendant_remains_monitored(self):
        snapshot = {1: self.record(1, 0), 2: self.record(2, 1, rss=2048),
                    3: self.record(3, 0, rss=4096), 99: self.record(99, 0, rss=8192)}
        tracked = {3: {"start_ticks": "1", "roots": ["outer"]}}
        with patch("stress_acceptance.process_snapshot", return_value=(snapshot, [])), patch("stress_acceptance.os.getpid", return_value=99):
            rss, identities, exits, proof, retained = sample_process_trees({"outer": 1, "inner": 2}, {}, tracked)
        self.assertEqual(sum(rss.values()), 1024 + 2048 + 4096 + 8192)
        self.assertEqual(set(identities["2"]["roots"]), {"outer", "inner"})
        self.assertEqual(proof["unique_native_processes"], 3)
        self.assertEqual(proof["roots"]["outer"]["live_descendants"], 2)
        self.assertEqual(set(retained), {1, 2, 3})

    def test_root_reuse_exit_and_partial_scan_fail_closed(self):
        snapshot = {1: self.record(1, 0, started="changed"), 99: self.record(99, 0)}
        with patch("stress_acceptance.process_snapshot", return_value=(snapshot, [])), patch("stress_acceptance.os.getpid", return_value=99):
            with self.assertRaisesRegex(DriverError, "named_pid_identity_changed"):
                sample_process_trees({"service": 1}, {"service": "old"})
        snapshot[1]["state"] = "Z"
        with self.assertRaisesRegex(DriverError, "named_native_process_not_live"):
            select_process_trees(snapshot, {"service": 1})
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "123").mkdir()
            # Existing PID directory without readable stat is not an exited PID.
            with self.assertRaisesRegex(DriverError, "process_snapshot_incomplete"):
                process_snapshot(Path(directory))

    def test_nonexistent_children_api_does_not_hide_real_parent_child_processes(self):
        # Harmless same-exec OS test; no application/service is launched.
        child_code = "import os,time; memory=bytearray(8<<20); print(os.getpid(),flush=True); time.sleep(30)"
        parent_code = """import json,os,signal,subprocess,sys,time
child = subprocess.Popen([sys.executable, '-c', sys.argv[1]], stdout=subprocess.PIPE, text=True)
def stop(*args):
    raise SystemExit(0)
signal.signal(signal.SIGTERM, stop)
try:
    assert int(child.stdout.readline()) == child.pid
    print(json.dumps([os.getpid(), child.pid]), flush=True)
    time.sleep(30)
finally:
    child.terminate()
    child.wait(timeout=3)
"""
        process = subprocess.Popen([sys.executable, "-c", parent_code, child_code], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True)
        try:
            ready, _, _ = select.select([process.stdout], [], [], 5)
            self.assertTrue(ready, "local fixture parent did not become ready")
            parent, child = json.loads(process.stdout.readline())
            rss, identities, exits, proof, _ = sample_process_trees({"fixture_parent": parent}, {})
            self.assertIn(str(parent), rss)
            self.assertIn(str(child), rss)
            self.assertGreaterEqual(rss[str(child)], 8 << 20)
            self.assertEqual(identities[str(child)]["parent_pid"], parent)
            self.assertGreaterEqual(proof["roots"]["fixture_parent"]["live_descendants"], 1)
            self.assertTrue(proof["enumeration_complete"])
            self.assertEqual(proof["method"], "proc_stat_ppid_snapshot")
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                process.stdout.close()
                process.stderr.close()


class ConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.credentials = self.directory / "credentials.json"
        self.credentials.write_text(json.dumps({"owner": {"username": "synthetic-owner", "password": "DO-NOT-LOG-PASSWORD"}}))
        self.credentials.chmod(0o600)
        self.env = {"TEST_PUBLIC_URL": "http://127.0.0.1:8080", "TEST_OIDC_ISSUER": "http://127.0.0.1:8081/realms/test",
                    "TEST_CREDENTIALS_FILE": str(self.credentials)}
        self.config = Config.from_environment(self.env, True, False)
        self.config.timeout = 5
        self.runtime = {"schema_version": 1, "disposable": True, "exclusive_owner": True,
                        "public_url": self.config.public, "oidc_issuer": self.config.issuer,
                        "synthetic_username": "synthetic-owner", "storage_path": self.tmp.name,
                        "native_pids": {}, "postgres": {"host": "127.0.0.1", "port": 5432, "database": "test",
                                                         "user": "fixture", "psql": sys.executable}}
        self.runtime_file = self.directory / "runtime.json"
        self.runtime_file.write_text(json.dumps(self.runtime))
        self.runtime_file.chmod(0o600)

    def invoke(self, args, env=None):
        stdout = io.StringIO()
        output = self.directory / "output.json"
        with contextlib.redirect_stdout(stdout), patch.object(Transport, "request", side_effect=AssertionError("network prohibited")), \
             patch("stress_acceptance.subprocess.run", side_effect=AssertionError("process prohibited")):
            code = main(["--runtime-file", str(self.runtime_file), "--output", str(output), *args], self.env if env is None else env)
        return code, json.loads(stdout.getvalue()), output

    def test_exact_loopback_only_including_https(self):
        for url in ("http://127.0.0.1:8080", "https://[::1]:8443", "https://localhost:8443"):
            literal_loopback(url)
        for url in ("https://production.example", "http://127.0.0.1.evil", "http://user@127.0.0.1", "http://127.0.0.1?target=evil"):
            with self.subTest(url=url), self.assertRaises(DriverError):
                literal_loopback(url)

    def test_exact_localhost_actual_execution_verifies_all_resolved_addresses(self):
        local = (2, 1, 6, "", ("127.0.0.1", 8443))
        remote = (2, 1, 6, "", ("203.0.113.10", 8443))
        with patch("stress_acceptance.socket.getaddrinfo", return_value=[local]):
            literal_loopback("https://localhost:8443", resolve_localhost=True)
        with patch("stress_acceptance.socket.getaddrinfo", return_value=[local, remote]), self.assertRaisesRegex(DriverError, "exclusively_loopback"):
            literal_loopback("https://localhost:8443", resolve_localhost=True)
        for host in ("localhost.", "sub.localhost", "localhost.evil"):
            with self.assertRaises(DriverError):
                literal_loopback("https://" + host)

    def test_runtime_requires_owned_private_file(self):
        self.runtime_file.chmod(0o644)
        with self.assertRaises(DriverError):
            private_json(str(self.runtime_file))
        link = self.directory / "link"
        link.symlink_to(self.runtime_file)
        with self.assertRaises(DriverError):
            private_json(str(link))

    def test_runtime_rejects_remote_database_mismatch_and_missing_metrics(self):
        for mutation in (lambda r: r.update(public_url="http://127.0.0.1:9090"),
                         lambda r: r.update(exclusive_owner=False), lambda r: r.pop("postgres"),
                         lambda r: r["postgres"].update(host="10.0.0.1"),
                         lambda r: r["postgres"].update(database="x;DROP DATABASE x"),
                         lambda r: r.update(native_pids={"a": 123, "b": 123})):
            runtime = copy.deepcopy(self.runtime)
            mutation(runtime)
            with self.assertRaises(DriverError):
                validate_runtime(runtime, self.config)

    def test_check_only_zero_io_never_pass(self):
        code, report, output = self.invoke(["--check-only"])
        self.assertEqual(code, 3)
        self.assertEqual(report["execution"], "not_run")
        self.assertEqual(report["status"], "not_run")
        self.assertEqual(report["invariants"]["status"], "not_run")
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        self.assertNotIn("DO-NOT-LOG-PASSWORD", output.read_text())
        self.assertNotIn("synthetic-owner", output.read_text())
        self.assertNotIn("127.0.0.1", output.read_text())

    def test_extended_cli_preflight_reports_plan_not_execution(self):
        code, report, _ = self.invoke(["--check-only", "--max-concurrency", "64"])
        self.assertEqual(code, 3)
        self.assertEqual(report["requested_concurrency_schedule"], [1, 4, 8, 16, 32, 64])
        self.assertEqual(report["maximum_requested_concurrency"], 64)
        self.assertEqual(report["stages"], [])
        self.assertEqual(report["execution"], "not_run")

    def test_run_reports_actual_extended_schedule_and_distinct_throughput(self):
        def stage(suite, number, concurrency):
            suite.report["stages"].append({"stage": number, "concurrency": concurrency,
                                           "status": "pass", "timed_load_seconds": 1})
            suite.metrics.record("http", number, "stat", .1, status=200, phase="timed")
            suite.metrics.record("http", number, "stage_audit", .1, status=200, phase="audit")
        with patch.object(Suite, "setup"), patch.object(Suite, "monitor"), \
             patch.object(Suite, "stage", stage), patch.object(Suite, "final_audit"):
            report = run(self.config, self.runtime, Options(maximum_concurrency=64),
                         allow_disposable_writes=True, oidc_consent=True)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["executed_concurrency_schedule"], [1, 4, 8, 16, 32, 64])
        self.assertEqual(report["maximum_executed_concurrency"], 64)
        self.assertEqual(report["budgets"]["maximum_scheduled_concurrency"], 64)
        self.assertEqual(report["http"]["count"], 12)
        self.assertEqual(report["timed_http"]["count"], 6)
        self.assertEqual(report["timed_http"]["throughput_per_second"], 1)
        self.assertNotEqual(report["http"]["throughput_basis"], report["timed_http"]["throughput_basis"])

    def test_extended_stop_does_not_claim_unreached_concurrency(self):
        def stage(suite, number, concurrency):
            suite.report["stages"].append({"stage": number, "concurrency": concurrency, "status": "stopped"})
            suite.safety.abort("sampled_rss_exceeded_4gib")
            suite.safety.guard()
        with patch.object(Suite, "setup"), patch.object(Suite, "monitor"), patch.object(Suite, "stage", stage):
            report = run(self.config, self.runtime, Options(maximum_concurrency=64),
                         allow_disposable_writes=True, oidc_consent=True)
        self.assertEqual(report["status"], "stopped")
        self.assertEqual(report["requested_concurrency_schedule"], [1, 4, 8, 16, 32, 64])
        self.assertEqual(report["executed_concurrency_schedule"], [1])
        self.assertEqual(report["maximum_executed_concurrency"], 1)

    def test_actual_cli_needs_write_optin(self):
        code, report, _ = self.invoke([])
        self.assertEqual(code, 2)
        self.assertEqual(report["reason"], "explicit_disposable_write_optin_required")
        self.assertEqual(report["status"], "not_run")

    def test_actual_cli_needs_oidc_consent(self):
        code, report, _ = self.invoke(["--allow-disposable-writes"])
        self.assertEqual(code, 2)
        self.assertEqual(report["reason"], "explicit_disposable_oidc_consent_required")

    def test_importable_run_also_requires_explicit_authority(self):
        with patch.object(Suite, "setup", side_effect=AssertionError("must not run")):
            with self.assertRaisesRegex(DriverError, "write_optin"):
                run(self.config, self.runtime)
            with self.assertRaisesRegex(DriverError, "oidc_consent"):
                run(self.config, self.runtime, allow_disposable_writes=True)

    def test_output_cannot_replace_credentials_or_runtime(self):
        before = self.credentials.read_bytes()
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            code = main(["--runtime-file", str(self.runtime_file), "--output", str(self.credentials), "--check-only"], self.env)
        self.assertEqual(code, 2)
        self.assertEqual(self.credentials.read_bytes(), before)

    def test_existing_failed_output_retained(self):
        out = self.directory / "output.json"
        out.write_text("original failed evidence")
        code, report, _ = self.invoke(["--check-only"])
        self.assertEqual(code, 2)
        self.assertEqual(report["reason"], "report_output_must_be_new")
        self.assertEqual(out.read_text(), "original failed evidence")

    def test_setup_failure_never_pass_or_leak_raw_exception(self):
        with patch.object(Suite, "setup", side_effect=RuntimeError("secret upstream body password=raw")):
            report = run(self.config, self.runtime, allow_disposable_writes=True, oidc_consent=True)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["reason"], "internal_driver_failure")
        self.assertNotIn("upstream", json.dumps(report))
        self.assertEqual(report["invariants"]["status"], "not_verified")
        self.assertIsNone(report["http"]["p99_seconds"])

    def test_missing_stage_evidence_never_pass(self):
        def fake_setup(suite):
            suite.baseline = (0, 0)
        with patch.object(Suite, "setup", fake_setup), patch.object(Suite, "monitor"), \
             patch.object(Suite, "stage"), patch.object(Suite, "final_audit"):
            report = run(self.config, self.runtime, allow_disposable_writes=True, oidc_consent=True)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["reason"], "missing_stage_evidence")

    def test_late_monitor_abort_revokes_success(self):
        def fake_stage(suite, number, concurrency):
            suite.report["stages"].append({"stage": number, "status": "pass"})
        def fake_audit(suite):
            suite.report["invariants"] = {"status": "pass"}
            suite.safety.abort("sampled_rss_exceeded_4gib")
        with patch.object(Suite, "setup"), patch.object(Suite, "monitor"), \
             patch.object(Suite, "stage", fake_stage), patch.object(Suite, "final_audit", fake_audit):
            report = run(self.config, self.runtime, allow_disposable_writes=True, oidc_consent=True)
        self.assertEqual(report["status"], "stopped")
        self.assertEqual(report["invariants"]["status"], "not_verified")

    def test_pg_metrics_fixed_readonly_and_error_body_discarded(self):
        safety = Safety(self.runtime, Options(), Metrics())
        with patch("stress_acceptance.subprocess.run", return_value=SimpleNamespace(returncode=1, stdout="", stderr="raw secret")) as call:
            with self.assertRaisesRegex(DriverError, "postgres_metrics_unavailable") as error:
                safety.sample()
        self.assertNotIn("raw secret", str(error.exception))
        args = call.call_args
        self.assertIn("--no-psqlrc", args.args[0])
        self.assertEqual(args.args[0][-1], PG_SQL)
        self.assertIn("default_transaction_read_only=on", args.kwargs["env"]["PGOPTIONS"])
        self.assertNotIn("PGPASSWORD", args.kwargs["env"])
        self.assertLessEqual(args.kwargs["timeout"], 3)

    def test_resource_sample_explicit_native_not_checked(self):
        safety = Safety(self.runtime, Options(), Metrics())
        with patch("stress_acceptance.subprocess.run", return_value=SimpleNamespace(returncode=0, stdout=json.dumps({k: 0 for k in PG_FIELDS}))), \
             patch("stress_acceptance.threshold_reason", return_value=None):
            sample = safety.sample()
        self.assertEqual(sample["native_rss_scope"], "not_checked")
        self.assertTrue(sample["metrics_complete"])
        self.assertGreater(sample["sampled_rss_bytes"], 0)

    def test_missing_named_process_fails_instead_of_zero_rss(self):
        runtime = copy.deepcopy(self.runtime)
        runtime["native_pids"] = {"nonexistent": 999999999}
        with self.assertRaisesRegex(DriverError, "named_native_process_not_live"):
            Safety(runtime, Options(), Metrics()).sample()

    def test_expected409_requires_exact_recognized_code(self):
        suite = Suite(self.config, self.runtime, Options(), new_report())
        suite.auth.access = "synthetic-not-used-on-network"
        suite.auth.expires = time.monotonic() + 300
        client = Session(suite, 1, "same_target_conflict")
        response = Response(409, {}, json.dumps({"error": {"code": "PATH_CONFLICT"}}).encode())
        with patch.object(client.api, "request", return_value=response):
            self.assertEqual(client.request("POST", "/v1/uploads", {}, conflict_codes=("PATH_CONFLICT",)).status, 409)
        self.assertEqual(suite.metrics.summary("http")["expected_409"], 1)
        self.assertEqual(suite.metrics.summary("http")["unexpected_errors"], 0)
        response.body = b'{"error":{"code":"UNKNOWN","message":"raw-secret"}}'
        with patch.object(client.api, "request", return_value=response), self.assertRaises(DriverError):
            client.request("POST", "/v1/uploads", {}, conflict_codes=("PATH_CONFLICT",))
        self.assertEqual(suite.metrics.summary("http")["unexpected_errors"], 1)
        self.assertNotIn("raw-secret", json.dumps(suite.metrics.http))

    def test_http200_with_corrupt_content_is_scenario_failure(self):
        suite = Suite(self.config, self.runtime, Options(), new_report())
        suite.invoke(1, "download", lambda _: (_ for _ in ()).throw(DriverError("download_bytes_mismatch")))
        self.assertTrue(suite.safety.stop.is_set())
        self.assertEqual(suite.metrics.summary("scenarios")["unexpected_errors"], 1)
        self.assertEqual(suite.safety.reason, "download_bytes_mismatch")

    def test_completed_stage_without_resource_samples_cannot_pass(self):
        suite = Suite(self.config, self.runtime, Options(stage_seconds=1), new_report())
        suite.conflict_file, suite.write_root, suite.batch_root = "fixture", "write", "batch"
        def fake_worker_metrics(*args, **kwargs):
            for workload in WORKLOADS:
                suite.metrics.record("scenarios", 1, workload, .01, phase="timed")
            time.sleep(.01)
        with patch.object(suite, "race", return_value={"status": "pass"}), \
             patch.object(suite, "invoke", side_effect=fake_worker_metrics), \
             patch.object(suite, "verify_file"), patch.object(suite, "verify_listing"), \
             patch.object(suite, "check_quota"), patch.object(suite, "drain_pipeline", return_value={"status": "pass"}), \
             patch.object(suite.safety, "guard"):
            # One-second timed window is intentionally real and remains bounded.
            with self.assertRaisesRegex(DriverError, "stage_resource_evidence_missing"):
                suite.stage(1, 1)
        data = suite.report["stages"][0]
        self.assertEqual(data["status"], "fail")
        self.assertEqual(data["resources"]["sample_count"], 0)
        self.assertIsNone(data["resources"]["peak_sampled_rss_bytes"])
        self.assertLessEqual(data["start_elapsed_seconds"], data["timed_start_elapsed_seconds"])
        self.assertLessEqual(data["timed_end_elapsed_seconds"], data["end_elapsed_seconds"])

    def test_race_failure_still_retains_stage_statistics(self):
        suite = Suite(self.config, self.runtime, Options(), new_report())
        with patch.object(suite, "race", side_effect=DriverError("race_requires_exactly_one_winner")):
            with self.assertRaises(DriverError):
                suite.stage(1, 1)
        stage = suite.report["stages"][0]
        self.assertEqual(stage["status"], "fail")
        self.assertEqual(set(stage["workloads"]), set(WORKLOADS))
        self.assertEqual(stage["timed_load_seconds"], 0)
        self.assertEqual(stage["http"]["count"], 0)


if __name__ == "__main__":
    unittest.main()
