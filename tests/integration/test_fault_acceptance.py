"""Deterministic OFFLINE guard tests with fake callbacks/responses, not live evidence.

No test connects to a service, binds a socket, sends a signal, starts a process,
modifies application tables, or generates a retained acceptance report.
"""
import contextlib
from email.message import Message
import io
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import fault_acceptance as f
from deployed_api_harness import Config, DriverError, Response, Transport

FID = "11111111-1111-4111-8111-111111111111"
UID = "22222222-2222-4222-8222-222222222222"
VID = "33333333-3333-4333-8333-333333333333"
GATES = {"ALLOW_DISPOSABLE_TEST_WRITES": "yes", "ALLOW_DISPOSABLE_TEST_FAULTS": "yes",
         "ALLOW_DISPOSABLE_TEST_OIDC_CONSENT": "yes"}


def fixture_config():
    return Config("http://127.0.0.1:8080", "http://127.0.0.1:8081/realms/fixture",
                  {"owner": {"username": "OFFLINE_OWNER", "password": "OFFLINE_SECRET"}})


def fixture_snapshot():
    return {"file_count": 1, "version_count": 1, "upload_count": 1, "committed_reservations": 1,
            "held_reservations": 0, "ready_artifact_count": 0, "unpublished_outbox_count": 1,
            "max_unpublished_outbox_attempt": 0,
            "job_states": ["queued"], "job_attempts": [0]}


class FakeTimer:
    """OFFLINE timer fixture: tests invoke callbacks explicitly, without waiting."""
    def __init__(self, interval, callback, kwargs):
        self.interval, self.callback, self.kwargs = interval, callback, kwargs
        self.daemon = False
        self.cancelled = False

    def start(self):
        pass

    def cancel(self):
        self.cancelled = True


class FakeProcess:
    """OFFLINE process state only; never wraps or signals a real PID."""
    def __init__(self):
        self.active = True
        self.stops = self.starts = 0

    def stop(self):
        self.stops += 1
        self.active = False

    def start(self):
        self.starts += 1
        self.active = True

    def control(self):
        return f.ServiceControl(self.stop, self.start, lambda: self.active)


class GuardTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = str(Path(self.tmp.name) / "report.json")
        self.network = patch.object(Transport, "request", side_effect=AssertionError("OFFLINE network forbidden"))
        self.network.start()

    def tearDown(self):
        self.network.stop()
        self.tmp.cleanup()

    def test_check_only_never_calls_network_or_controls(self):
        controls = f.ServiceControl(Mock(side_effect=AssertionError), Mock(side_effect=AssertionError), Mock(side_effect=AssertionError))
        rc, report = f.run(fixture_config(), self.path, f.Hooks({"worker": controls}), check_only=True)
        self.assertEqual(rc, 3)
        self.assertEqual(report["execution"], "not_run")
        self.assertTrue(all(c["status"] == "not_run" for c in report["cases"]))
        self.assertEqual(os.stat(self.path).st_mode & 0o777, 0o600)
        controls.running.assert_not_called()

    def test_loopback_enforced_even_for_https(self):
        for url in ("https://prod.example", "http://127.0.0.1.example", "https://user@127.0.0.1", "https://[::1]/oops"):
            with self.subTest(url=url), self.assertRaises(DriverError):
                f.local_url(url, base=True)
        for url in ("http://127.0.0.1:8080", "https://localhost", "https://[::1]:8443"):
            self.assertEqual(f.local_url(url, base=True), url)

    def test_issuer_and_optional_meili_must_be_loopback(self):
        for attr, value in (("issuer", "https://idp.example/realms/x"), ("meili_url", "https://search.example")):
            config = fixture_config()
            setattr(config, attr, value)
            with self.subTest(attr=attr), self.assertRaises(DriverError):
                f.validate_config(config)

    def test_each_gate_is_required_before_login(self):
        for gate in GATES:
            env = {k: v for k, v in GATES.items() if k != gate}
            with self.subTest(gate=gate), patch.object(f.MeasuredAPI, "login") as login:
                rc, report = f.run(fixture_config(), self.path + gate, env=env)
                self.assertEqual(rc, 2)
                self.assertEqual(report["execution"], "not_run")
                login.assert_not_called()

    def test_private_new_report_is_mandatory(self):
        rc, report = f.run(fixture_config(), None, env=GATES)
        self.assertEqual((rc, report["reason"]), (2, "new_private_report_required"))

    def test_existing_report_cannot_be_overwritten_and_precedes_login(self):
        Path(self.path).write_text("original")
        with patch.object(f.MeasuredAPI, "login") as login:
            rc, report = f.run(fixture_config(), self.path, env=GATES)
        self.assertEqual(rc, 2)
        self.assertEqual(Path(self.path).read_text(), "original")
        login.assert_not_called()

    def test_symlink_report_is_never_followed(self):
        target = Path(self.tmp.name) / "original"
        target.write_text("original")
        Path(self.path).symlink_to(target)
        rc, _ = f.run(fixture_config(), self.path, env=GATES)
        self.assertEqual(rc, 2)
        self.assertEqual(target.read_text(), "original")

    def test_control_allowlist_rejects_host_services_and_shell_strings(self):
        for hooks in (f.Hooks({"postgres": FakeProcess().control()}), f.Hooks({"worker": "kill something"}),
                      f.Hooks(snapshot="shell command")):
            with self.subTest(hooks=type(hooks)), self.assertRaises(DriverError):
                hooks.validate()

    def test_missing_controls_are_not_run_never_skipped_pass(self):
        rc, report = f.run(fixture_config(), self.path, env=GATES)
        self.assertEqual(rc, 3)
        self.assertEqual(report["status"], "not_run")
        self.assertTrue(all(c["status"] == "not_run" for c in report["cases"]))
        move = report["cases"][-1]
        self.assertEqual(move["reason"], "stable_native_gateway_and_history_proof_not_provided")

    def test_only_native_move_does_not_login_or_write_fixture(self):
        with patch.object(f.MeasuredAPI, "login") as login:
            rc, report = f.run(fixture_config(), self.path, env=GATES, scenarios=["native_move_response_loss"])
        self.assertEqual(rc, 3)
        self.assertEqual(report["execution"], "not_run")
        login.assert_not_called()

    def test_reports_exclude_secrets_ids_and_origins(self):
        _, report = f.run(fixture_config(), self.path, env=GATES, check_only=True)
        saved = Path(self.path).read_text()
        self.assertEqual(json.loads(saved), report)
        for forbidden in ("OFFLINE_OWNER", "OFFLINE_SECRET", "127.0.0.1", FID, VID):
            self.assertNotIn(forbidden, saved)

    def test_arbitrary_driver_error_is_not_a_safe_code(self):
        self.assertEqual(f.failure_code(DriverError("SecretValueWithNoSpaces")), "fault_case_failed_details_suppressed")
        self.assertEqual(f.failure_code(ValueError("token secret")), "fault_case_failed_details_suppressed")
        self.assertEqual(f.failure_code(DriverError("service_recovery_failed")), "service_recovery_failed")

    def test_partial_never_becomes_full_acceptance(self):
        report = f.new_report(list(f.SCENARIOS[:3]))
        for case in report["cases"][:3]:
            case["status"] = "pass"  # OFFLINE summary fixture, not an execution.
        self.assertEqual(f.summarize(report, list(f.SCENARIOS[:3])), 3)
        self.assertEqual(report["selected_status"], "pass")
        self.assertEqual(report["full_fault_acceptance"], "partial")
        report["cases"][1]["status"] = "fail"
        self.assertEqual(f.summarize(report, list(f.SCENARIOS[:3])), 1)

    def test_one_case_failure_preserves_independent_results(self):
        controls = {name: FakeProcess().control() for name in f.SERVICES}
        hooks = f.Hooks(controls, lambda *_: fixture_snapshot())
        def offline_case(suite, service):
            if service == "nats":
                raise DriverError("recovery_deadline_exceeded")
            suite.check(True, "offline_orchestration_fixture_only")
        with patch.object(f.MeasuredAPI, "login"), patch.object(f.MeasuredAPI, "delegate"), \
                patch.object(f.Suite, "interrupted_service", offline_case):
            rc, report = f.run(fixture_config(), self.path, hooks, env=GATES, scenarios=list(f.SCENARIOS[:3]))
        self.assertEqual(rc, 1)
        self.assertEqual([c["status"] for c in report["cases"]], ["pass", "fail", "pass", "not_run", "not_run"])
        self.assertEqual(report["cases"][1]["reason"], "recovery_deadline_exceeded")

    def test_unconfirmed_recovery_stops_later_faults(self):
        process = FakeProcess()
        hooks = f.Hooks({"worker": process.control(), "nats": FakeProcess().control()}, lambda *_: fixture_snapshot())
        called = []
        def offline_case(suite, service):
            called.append(service)
            process.active = False
            raise ValueError("SECRET fixture failure")
        with patch.object(f.MeasuredAPI, "login"), patch.object(f.MeasuredAPI, "delegate"), \
                patch.object(f.Suite, "interrupted_service", offline_case):
            rc, report = f.run(fixture_config(), self.path, hooks, env=GATES, scenarios=list(f.SCENARIOS[:2]))
        self.assertEqual(rc, 1)
        self.assertEqual(called, ["worker"])
        self.assertEqual(report["cases"][0]["reason"], "service_recovery_unconfirmed")
        self.assertEqual(report["cases"][1]["status"], "not_run")
        self.assertEqual(report["cases"][1]["reason"], "prior_service_recovery_unconfirmed")
        self.assertNotIn("SECRET", json.dumps(report))

    def test_metrics_do_not_mix_injected_and_unexpected_availability(self):
        api = f.MeasuredAPI(fixture_config())
        api.metrics = {"availability_errors_during_injected_downtime": 0, "unexpected_availability_errors": 0}
        api.injecting = True
        api.availability_error()
        api.injecting = False
        api.availability_error()
        self.assertEqual(api.metrics, {"availability_errors_during_injected_downtime": 1,
                                      "unexpected_availability_errors": 1})


class SnapshotTests(unittest.TestCase):
    def test_valid_offline_snapshot(self):
        self.assertEqual(f.validated_snapshot(fixture_snapshot()), fixture_snapshot())

    def test_rejects_extra_secret_fields_boolean_counts_and_unknown_states(self):
        changes = ({"secret": "do not export"}, {"file_count": True}, {"version_count": -1},
                   {"job_states": ["complete"]}, {"job_attempts": []}, {"job_attempts": [True]})
        for change in changes:
            with self.subTest(change=change), self.assertRaises(DriverError):
                f.validated_snapshot({**fixture_snapshot(), **change})

    def test_duplicate_versions_or_quota_settlements_fail(self):
        suite = f.Suite(fixture_config(), f.Hooks())
        suite.case = {"checks": []}
        suite.quota = lambda: (100, 0)
        fixture = {"baseline": (90, 0), "payload": b"1234567890"}
        for key in ("file_count", "version_count", "upload_count", "committed_reservations", "held_reservations"):
            with self.subTest(key=key), self.assertRaises(DriverError):
                suite.exact_storage(fixture, {**fixture_snapshot(), key: 2}, ready=False)

    def test_ready_claim_requires_single_job_artifact_and_drained_outbox(self):
        suite = f.Suite(fixture_config(), f.Hooks())
        suite.case = {"checks": []}
        suite.quota = lambda: (10, 0)
        fixture = {"baseline": (0, 0), "payload": b"1234567890"}
        for changes in ({}, {"job_states": ["ready"]},
                        {"job_states": ["ready"], "ready_artifact_count": 1}):
            with self.subTest(changes=changes), self.assertRaises(DriverError):
                suite.exact_storage(fixture, {**fixture_snapshot(), **changes}, ready=True)

    def test_search_artifact_must_match_actual_published_file_pointer(self):
        suite = f.Suite(fixture_config(), f.Hooks())
        suite.case = {"checks": [], "metrics": {"unexpected_availability_errors": 0}}
        suite.snapshot = lambda _: {**fixture_snapshot(), "job_states": ["ready"],
                                   "ready_artifact_count": 1, "unpublished_outbox_count": 0}
        suite.quota = lambda: (10, 0)
        suite.owner = Mock()
        suite.owner.request.return_value = Response(200, {}, json.dumps({"items": [{"artifact_set_id": UID}]}).encode())
        suite.owner.json.return_value = {"current_version_id": VID, "conversion": {"status": "ready"},
                                        "search_artifact_set_id": FID}
        fixture = {"file_id": FID, "version_id": VID, "name": "OFFLINE.txt", "marker": "OFFLINE",
                   "payload": b"1234567890", "baseline": (0, 0)}
        with patch.object(f, "search_matches", return_value=True), self.assertRaises(DriverError):
            suite.recover_and_verify(fixture)
        self.assertEqual(suite.case["checks"][-1],
                         {"name": "search_artifact_matches_published_file_pointer", "status": "fail"})


class RecoveryGuardTests(unittest.TestCase):
    def setUp(self):
        self.timer = patch.object(f.threading, "Timer", FakeTimer)
        self.timer.start()

    def tearDown(self):
        self.timer.stop()

    def test_restores_on_success_and_once_only(self):
        process, metrics = FakeProcess(), {}
        with f.Interruption(process.control(), metrics) as fault:
            self.assertFalse(process.active)
        fault.recover()
        self.assertTrue(process.active)
        self.assertEqual((process.stops, process.starts), (1, 1))
        self.assertLess(metrics["injected_downtime_seconds"], 10)

    def test_restores_on_assertion_and_keyboard_interrupt(self):
        for error in (ValueError("OFFLINE failure"), KeyboardInterrupt()):
            process = FakeProcess()
            with self.subTest(error=type(error)), self.assertRaises(type(error)):
                with f.Interruption(process.control(), {}):
                    raise error
            self.assertTrue(process.active)

    def test_start_failure_cannot_pass(self):
        process = FakeProcess()
        control = f.ServiceControl(process.stop, Mock(side_effect=ValueError("SECRET")), lambda: process.active)
        with self.assertRaisesRegex(DriverError, "service_recovery_failed"):
            with f.Interruption(control, {}):
                pass

    def test_stop_error_attempts_restoration_and_suppresses_raw_error(self):
        process = FakeProcess()
        def bad_stop():
            process.stop()
            raise ValueError("SECRET")
        control = f.ServiceControl(bad_stop, process.start, lambda: process.active)
        with self.assertRaisesRegex(DriverError, "service_stop_failed_or_bound_exceeded"):
            with f.Interruption(control, {}):
                self.fail("must not enter")
        self.assertTrue(process.active)

    def test_watchdog_restores_but_expired_injection_cannot_pass(self):
        process = FakeProcess()
        with self.assertRaisesRegex(DriverError, "injection_window_expired"):
            with f.Interruption(process.control(), {}) as fault:
                fault.timer.callback(**fault.timer.kwargs)
                self.assertTrue(process.active)

    def test_fault_cannot_start_on_already_absent_service(self):
        process = FakeProcess()
        process.active = False
        with self.assertRaisesRegex(DriverError, "fixture_service_not_running"):
            with f.Interruption(process.control(), {}):
                pass
        self.assertEqual(process.stops, 0)


class LossProxyOfflineTests(unittest.TestCase):
    def handler(self, response=None):
        """Constructs only an in-memory handler; never instantiate HTTPServer."""
        server = SimpleNamespace(path="/v1/uploads/" + UID + "/finalize", nonce="OFFLINE_NONCE", attempts=0,
                                 forwarded=0, dropped=0, result=None, failure=False, upstream_status=None,
                                 finished=Mock(), forward=Mock(return_value=response or Response(200, {},
                                 json.dumps({"status": "saved", "file_id": FID, "version_id": VID}).encode())))
        handler = f.FinalizeLossHandler.__new__(f.FinalizeLossHandler)
        handler.server, handler.path = server, server.path
        handler.headers = Message()
        handler.headers["X-Fault-Fixture"] = server.nonce
        handler.headers["Content-Length"] = "2"
        handler.rfile, handler.wfile = io.BytesIO(b"{}"), io.BytesIO()
        handler.connection = Mock()
        handler.send_error = Mock()
        return handler, server

    def test_real_success_branch_drops_before_any_bytes_in_offline_fixture(self):
        handler, server = self.handler()
        handler.do_POST()
        self.assertEqual((server.attempts, server.forwarded, server.dropped), (1, 1, 1))
        self.assertEqual(handler.wfile.getvalue(), b"")
        handler.send_error.assert_not_called()
        handler.connection.shutdown.assert_called_once_with(socket.SHUT_RDWR)
        self.assertEqual(server.result, {"file_id": FID, "version_id": VID})

    def test_error_or_unknown_success_never_counts_as_drop(self):
        for response in (Response(503, {}, b"SECRET"), Response(200, {}, b'{"status":"outcome_unknown"}')):
            handler, server = self.handler(response)
            handler.do_POST()
            self.assertTrue(server.failure)
            self.assertEqual(server.dropped, 0)

    def test_duplicate_attempt_is_never_forwarded(self):
        handler, server = self.handler()
        server.forwarded = 1
        handler.do_POST()
        server.forward.assert_not_called()
        self.assertEqual(server.attempts, 1)
        self.assertEqual(server.dropped, 0)

    def test_wrong_path_nonce_framing_and_credentials_are_denied(self):
        for mutation in (lambda h: setattr(h, "path", "/v1/files/other"),
                         lambda h: h.headers.add_header("X-Fault-Fixture", "wrong"),
                         lambda h: h.headers.add_header("Content-Length", "2"),
                         lambda h: h.headers.add_header("Transfer-Encoding", "chunked"),
                         lambda h: h.headers.add_header("Authorization", "Bearer SECRET")):
            handler, server = self.handler()
            mutation(handler)
            handler.do_POST()
            server.forward.assert_not_called()
            self.assertEqual(server.dropped, 0)

    def test_expect_never_emits_interim_success(self):
        handler, _ = self.handler()
        self.assertFalse(handler.handle_expect_100())
        handler.send_error.assert_called_once_with(417)


if __name__ == "__main__":
    unittest.main()
