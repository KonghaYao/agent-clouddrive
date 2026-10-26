#!/usr/bin/env python3
"""Bounded, opt-in REAL local fault acceptance; imports/check-only do no I/O.

Runtime process controls are trusted Python callbacks supplied by the fixture
supervisor that owns its Popen handles. There are no shell commands, PID files,
dynamic imports, production service controls, or fabricated acceptance results.
"""
from __future__ import annotations

import argparse
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import ipaddress
import json
import os
from pathlib import Path
import secrets
import socket
import sys
import threading
import time
import urllib.parse
from typing import Callable

from deployed_api_harness import API, Config, DriverError, Transport, require, safe_id, validate_url
from deployed_api_acceptance import reserve_report, search_matches

SCENARIOS = ("worker_restart", "nats_recovery", "meili_recovery", "lost_finalize_response", "native_move_response_loss")
SERVICES = frozenset(("worker", "nats", "meili"))
COUNTS = frozenset(("file_count", "version_count", "upload_count", "committed_reservations",
                    "held_reservations", "ready_artifact_count", "unpublished_outbox_count",
                    "max_unpublished_outbox_attempt"))
JOB_STATES = frozenset(("queued", "processing", "ready", "partial", "not_converted", "blocked", "failed"))
SAFE_FAILURES = frozenset((
    "fault_assertion_failed", "read_only_snapshot_failed", "invalid_snapshot_schema", "invalid_snapshot_counts",
    "invalid_snapshot_jobs", "recovery_deadline_exceeded", "service_probe_failed", "fixture_service_not_running",
    "service_stop_failed_or_bound_exceeded", "service_recovery_failed", "service_interruption_bound_exceeded",
    "injection_window_expired_service_restored", "invalid_resource_id", "invalid_quota_response",
    "network_or_tls_failure_outcome_unconfirmed", "invalid_json_response", "expected_json_object",
    "invalid_search_response", "invalid_search_hit", "authorization_http_failure", "oidc_discovery_failed",
    "token_exchange_failed", "delegation_callback_failed", "delegation_not_enabled", "oidc_issuer_mismatch",
    "oidc_endpoint_mismatch", "invalid_scenario_selection", "new_private_report_required",
    "report_cannot_replace_credentials", "report_output_must_be_new", "report_write_failed", "invalid_endpoint",
    "invalid_endpoint_port", "https_required", "public_url_requires_origin", "invalid_endpoint_path",
    "faults_require_loopback_origins", "keycloak_realm_issuer_required", "fixture_owner_required",
    "control_service_not_allowlisted", "invalid_service_control", "invalid_snapshot_hook",
    "disposable_write_opt_in_required", "disposable_fault_opt_in_required", "disposable_oidc_consent_opt_in_required",
))


def failure_code(exc):
    code = str(exc) if isinstance(exc, DriverError) else ""
    return code if code in SAFE_FAILURES else "fault_case_failed_details_suppressed"


def local_url(value, *, base=False):
    value = validate_url(value, loopback_http=True, base=base)
    hostname = urllib.parse.urlsplit(value).hostname
    try:
        local = ipaddress.ip_address(hostname).is_loopback
    except ValueError:
        local = hostname == "localhost"
    require(local, "faults_require_loopback_origins")
    return value


def validate_config(config):
    local_url(config.public, base=True)
    local_url(config.issuer)
    require("/realms/" in urllib.parse.urlsplit(config.issuer).path, "keycloak_realm_issuer_required")
    if config.meili_url:
        local_url(config.meili_url, base=True)
    require(isinstance(config.credentials, dict) and isinstance(config.credentials.get("owner"), dict),
            "fixture_owner_required")


@dataclass(repr=False)
class ServiceControl:
    """Supervisor-owned processes only; stop/start each finish within two seconds.

    stop sends graceful SIGTERM and verifies exit, never SIGKILL or SIGSTOP.
    start reuses the exact fixture argv/environment/data directory and does not
    create another process if the original is still running. running is read-only.
    Callbacks must be thread-safe: the independent restoration watchdog calls start.
    """
    stop: Callable[[], None]
    start: Callable[[], None]
    running: Callable[[], bool]


@dataclass(repr=False)
class Hooks:
    services: dict[str, ServiceControl] = field(default_factory=dict)
    snapshot: Callable[[str, str], dict] | None = None

    def validate(self):
        require(isinstance(self.services, dict) and set(self.services) <= SERVICES, "control_service_not_allowlisted")
        for control in self.services.values():
            require(isinstance(control, ServiceControl) and all(callable(getattr(control, k))
                    for k in ("stop", "start", "running")), "invalid_service_control")
        require(self.snapshot is None or callable(self.snapshot), "invalid_snapshot_hook")


def validated_snapshot(value):
    require(isinstance(value, dict) and set(value) == COUNTS | {"job_states", "job_attempts"},
            "invalid_snapshot_schema")
    require(all(type(value[k]) is int and 0 <= value[k] <= 1000000 for k in COUNTS), "invalid_snapshot_counts")
    states, attempts = value["job_states"], value["job_attempts"]
    require(isinstance(states, list) and isinstance(attempts, list) and len(states) == len(attempts)
            and len(states) <= 100 and all(isinstance(s, str) and s in JOB_STATES for s in states)
            and all(type(a) is int and 0 <= a <= 100 for a in attempts), "invalid_snapshot_jobs")
    return value


class Interruption:
    """Restore even if an assertion/network call fails; watchdog is independent.

    Maximum envelope: stop <=2s, armed interval <=6s, start <=2s. A bound breach
    fails acceptance, even if eventual restoration succeeds. This is not a crash
    simulator and never kills or signals a process itself.
    """
    def __init__(self, control, metrics):
        self.control, self.metrics = control, metrics
        self.lock = threading.Lock()
        self.restored = False
        self.restore_error = False
        self.timer = None
        self.watchdog_fired = False
        self.stopped = False

    def recover(self, watchdog=False):
        with self.lock:
            if self.restored:
                return
            self.watchdog_fired = self.watchdog_fired or watchdog
            try:
                self.control.start()
                require(self.control.running() is True, "service_restart_not_running")
                self.restored = True
            except Exception:
                self.restore_error = True
            self.metrics["injected_downtime_seconds"] = round(time.monotonic() - self.started, 4)

    def __enter__(self):
        try:
            require(self.control.running() is True, "fixture_service_not_running")
        except DriverError:
            raise
        except Exception:
            raise DriverError("service_probe_failed") from None
        self.started = time.monotonic()
        try:
            self.control.stop()
            require(self.control.running() is False, "service_exit_not_observed")
            self.stopped = True
            require(time.monotonic() - self.started <= 2, "service_stop_bound_exceeded")
            self.timer = threading.Timer(6, self.recover, kwargs={"watchdog": True})
            self.timer.daemon = True
            self.timer.start()
            return self
        except BaseException as exc:
            # A stop callback can fail after actually stopping its owned process.
            self.recover()
            if isinstance(exc, KeyboardInterrupt):
                raise
            raise DriverError("service_stop_failed_or_bound_exceeded") from None

    def __exit__(self, typ, value, tb):
        if self.timer:
            self.timer.cancel()
        self.recover()
        require(self.restored and not self.restore_error, "service_recovery_failed")
        require(self.metrics["injected_downtime_seconds"] <= 10, "service_interruption_bound_exceeded")
        require(not self.watchdog_fired, "injection_window_expired_service_restored")
        return False


class MeasuredAPI(API):
    def __init__(self, config):
        super().__init__(config)
        self.metrics = None
        self.injecting = False

    def request(self, *args, **kwargs):
        try:
            response = super().request(*args, **kwargs)
        except (DriverError, http.client.HTTPException):
            self.availability_error()
            raise
        if response.status >= 500:
            self.availability_error()
        return response

    def availability_error(self):
        if self.metrics is not None:
            key = ("availability_errors_during_injected_downtime" if self.injecting
                   else "unexpected_availability_errors")
            self.metrics[key] += 1


class FinalizeLossServer(HTTPServer):
    """One exact authenticated application finalize, EOF before any client bytes.

    The local caller supplies a one-use nonce, not a bearer token. The forwarding
    callback invokes the already-authenticated real API client on its fixed origin.
    No redirect, request-supplied URL, user-supplied code or arbitrary path is used.
    """
    allow_reuse_address = False

    def __init__(self, forward, path):
        self.forward, self.path = forward, path
        self.nonce = secrets.token_urlsafe(32)
        self.attempts = self.forwarded = self.dropped = 0
        self.upstream_status = None
        self.result = None
        self.finished = threading.Event()
        self.failure = False
        super().__init__(("127.0.0.1", 0), FinalizeLossHandler)

    def handle_error(self, *args):
        self.failure = True


class FinalizeLossHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def send_error(self, code, *args, **kwargs):
        self.close_connection = True
        self.send_response_only(code)
        self.send_header("Content-Length", "0")
        self.send_header("Connection", "close")
        self.end_headers()

    def handle_expect_100(self):
        self.send_error(417)
        return False

    def do_POST(self):
        self.close_connection = True
        self.connection.settimeout(3)
        server = self.server
        try:
            require(self.path == server.path and self.headers.get_all("X-Fault-Fixture") == [server.nonce]
                    and self.headers.get_all("Content-Length") == ["2"]
                    and not self.headers.get_all("Transfer-Encoding") and not self.headers.get_all("Expect")
                    and not self.headers.get_all("Authorization") and self.rfile.read(2) == b"{}",
                    "invalid_loss_proxy_request")
            server.attempts += 1
            require(server.forwarded == 0, "finalize_forwarding_already_consumed")
            server.forwarded += 1
            response = server.forward()
            server.upstream_status = response.status
            require(response.status == 200, "finalize_upstream_not_successful")
            result = response.json()
            require(result.get("status") in ("saved", "committed"), "finalize_upstream_not_committed")
            server.result = {"file_id": safe_id(result.get("file_id")), "version_id": safe_id(result.get("version_id"))}
            # No send_response or downstream body occurred on this successful path.
            self.connection.shutdown(socket.SHUT_RDWR)
            server.dropped += 1
        except Exception:
            server.failure = True
            try:
                self.send_error(502)
            except OSError:
                pass
        finally:
            server.finished.set()


class Suite:
    def __init__(self, config, hooks):
        # The outage window must not be swallowed by a 120-second socket wait.
        self.config = replace(config, timeout=3)
        self.owner = MeasuredAPI(self.config)
        self.hooks = hooks
        self.case = None

    def check(self, condition, name):
        self.case["checks"].append({"name": name, "status": "pass" if condition else "fail"})
        require(condition, "fault_assertion_failed")

    def snapshot(self, fixture):
        try:
            value = self.hooks.snapshot(fixture["file_id"], fixture["upload_id"])
        except Exception:
            raise DriverError("read_only_snapshot_failed") from None
        return validated_snapshot(value)

    def quota(self):
        q = self.owner.json("GET", "/v1/me").get("quota", {})
        require(all(type(q.get(k)) is int for k in ("used_bytes", "reserved_bytes")), "invalid_quota_response")
        return q["used_bytes"], q["reserved_bytes"]

    def prepare(self):
        baseline = self.quota()
        self.check(baseline[1] == 0, "isolated_fixture_has_no_pending_quota")
        marker = "faultprobe" + secrets.token_hex(12)
        payload = (marker + " synthetic retained original\n").encode()
        name = "fault-" + secrets.token_hex(12) + ".txt"
        body = {"name": name, "parent_id": "", "size_bytes": len(payload), "content_type": "text/plain",
                "idempotency_key": "fault-" + secrets.token_hex(16)}
        upload = self.owner.json("POST", "/v1/uploads", body)
        fixture = {"file_id": safe_id(upload.get("file_id")), "upload_id": safe_id(upload.get("id")),
                   "body": body, "name": name, "marker": marker, "payload": payload, "baseline": baseline}
        response = self.owner.request("PATCH", "/v1/tus/" + fixture["upload_id"], payload,
                                      {"Tus-Resumable": "1.0.0", "Upload-Offset": "0",
                                       "Content-Type": "application/offset+octet-stream"})
        self.check(response.status == 204 and response.headers.get("Upload-Offset") == str(len(payload)),
                   "real_native_upload_bytes_acknowledged")
        return fixture

    def finalize(self, fixture):
        result = self.owner.json("POST", "/v1/uploads/" + fixture["upload_id"] + "/finalize", {})
        self.check(result.get("status") in ("saved", "committed") and result.get("file_id") == fixture["file_id"],
                   "original_commit_confirmed")
        fixture["version_id"] = safe_id(result.get("version_id"))
        return result

    def exact_storage(self, fixture, snapshot, *, ready):
        expected = {"file_count": 1, "version_count": 1, "upload_count": 1,
                    "committed_reservations": 1, "held_reservations": 0}
        self.check(all(snapshot[k] == v for k, v in expected.items()), "single_original_version_upload_and_quota_settlement")
        if ready:
            self.check(snapshot["job_states"] == ["ready"] and snapshot["ready_artifact_count"] == 1
                       and snapshot["unpublished_outbox_count"] == 0, "durable_job_and_outbox_drained_once")
        self.check(self.quota() == (fixture["baseline"][0] + len(fixture["payload"]), 0), "exact_latest_original_quota")

    def recover_and_verify(self, fixture):
        start = time.monotonic()
        deadline = start + 120
        fid, vid = fixture["file_id"], fixture["version_id"]
        expected = {fid: {"version_id": vid, "path": "/" + fixture["name"], "marker": fixture["marker"]}}
        while time.monotonic() < deadline:
            state = self.snapshot(fixture)
            response = self.owner.request("POST", "/v1/search", {"query": fixture["marker"], "limit": 100})
            if response.status == 200:
                result = response.json()
                if (state["job_states"] == ["ready"] and state["ready_artifact_count"] == 1
                        and state["unpublished_outbox_count"] == 0 and search_matches(result, expected)
                        and len(result["items"]) == 1):
                    break
            time.sleep(min(0.25, max(0, deadline - time.monotonic())))
        else:
            raise DriverError("recovery_deadline_exceeded")
        self.check(time.monotonic() <= deadline, "recovery_within_120_seconds")
        self.case["metrics"]["recovery_seconds"] = round(time.monotonic() - start, 4)
        self.exact_storage(fixture, state, ready=True)
        meta = self.owner.json("GET", "/v1/files/" + fid)
        self.check(meta.get("current_version_id") == vid and meta.get("conversion", {}).get("status") == "ready",
                   "no_false_completed_file_state")
        self.check(result["items"][0].get("artifact_set_id") == safe_id(meta.get("search_artifact_set_id")),
                   "search_artifact_matches_published_file_pointer")
        versions = self.owner.json("GET", "/v1/files/" + fid + "/versions").get("items", [])
        self.check(len(versions) == 1 and versions[0].get("id") == vid, "exact_retained_version_identity")
        for path, label in (("/content", "current_original_bytes_retained"),
                            ("/content?version_id=" + vid, "pinned_original_bytes_retained")):
            response = self.owner.request("GET", "/v1/files/" + fid + path)
            self.check(response.status == 200 and response.body == fixture["payload"], label)
        self.check(True, "search_exact_file_version_artifact_ids_and_single_hit")
        self.check(self.case["metrics"]["unexpected_availability_errors"] == 0,
                   "no_unexpected_api_availability_errors")

    def interrupted_service(self, service):
        fixture = self.prepare()
        self.owner.injecting = True
        try:
            with Interruption(self.hooks.services[service], self.case["metrics"]):
                self.finalize(fixture)
                state = self.snapshot(fixture)
                # Prove work actually encountered the unavailable dependency,
                # rather than stopping/restarting faster than its polling loop.
                observation_deadline = time.monotonic() + 3
                if service in ("nats", "meili"):
                    attempted = False
                    while time.monotonic() < observation_deadline:
                        attempted = (state["max_unpublished_outbox_attempt"] >= 1 if service == "nats" else
                                     any(s == "queued" and a >= 1 for s, a in zip(state["job_states"], state["job_attempts"])))
                        if attempted:
                            break
                        time.sleep(0.1)
                        state = self.snapshot(fixture)
                    self.check(attempted, "durable_work_attempt_observed_during_dependency_absence")
                self.exact_storage(fixture, state, ready=False)
                if service == "nats":
                    self.check(state["unpublished_outbox_count"] >= 1, "committed_outbox_pending_during_nats_absence")
                else:
                    self.check(bool(state["job_states"]) and "ready" not in state["job_states"]
                               and state["ready_artifact_count"] == 0, "no_false_ready_while_dependency_absent")
                self.check(True, "test_owned_service_exit_observed")
        finally:
            self.owner.injecting = False
        self.check(True, "test_owned_process_restarted_within_10_seconds")
        self.recover_and_verify(fixture)

    def lost_finalize_response(self):
        fixture = self.prepare()
        path = "/v1/uploads/" + fixture["upload_id"] + "/finalize"
        with FinalizeLossServer(lambda: self.owner.request("POST", path, {}), path) as server:
            server.timeout = 5
            thread = threading.Thread(target=server.handle_request, daemon=True)
            thread.start()
            conn = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=5)
            eof = False
            try:
                conn.request("POST", path, body=b"{}", headers={"X-Fault-Fixture": server.nonce})
                conn.getresponse()
            except http.client.RemoteDisconnected:
                eof = True
            except (OSError, http.client.HTTPException):
                pass
            finally:
                conn.close()
            thread.join(6)
            self.check(not thread.is_alive() and server.finished.is_set() and not server.failure and eof
                       and server.upstream_status == 200 and server.attempts == server.forwarded == server.dropped == 1,
                       "actual_committed_finalize_response_dropped_before_client_bytes")
            self.check(server.result["file_id"] == fixture["file_id"], "lost_response_preserves_original_file_identity")
            fixture["version_id"] = server.result["version_id"]
        self.case["metrics"]["injected_response_losses"] = 1
        before = self.snapshot(fixture)
        replay = self.owner.json("POST", "/v1/uploads", fixture["body"])
        self.check(replay.get("id") == fixture["upload_id"] and replay.get("file_id") == fixture["file_id"],
                   "same_idempotency_key_reuses_original_upload")
        result = self.owner.json("POST", path, {})
        self.check(result.get("status") in ("saved", "committed") and result.get("file_id") == fixture["file_id"]
                   and result.get("version_id") == fixture["version_id"], "lost_response_replay_reuses_committed_version")
        after = self.snapshot(fixture)
        self.check(all(before[k] == after[k] for k in ("file_count", "version_count", "upload_count",
                       "committed_reservations", "held_reservations")), "replay_creates_no_original_version_or_quota_duplicate")
        self.recover_and_verify(fixture)


def new_report(selected):
    return {"schema_version": 1, "suite": "bounded-local-faults", "execution": "not_run", "status": "not_run",
            "full_fault_acceptance": "not_run", "selected_status": "not_run",
            "started_at": datetime.now(timezone.utc).isoformat(),
            "metric_definitions": {
                "injected_downtime_seconds": "Process stop invocation to confirmed restarted process liveness; not dependency health or API availability.",
                "recovery_seconds": "Restarted process liveness to confirmed durable job/outbox drain and exact search visibility.",
                "availability_errors_during_injected_downtime": "API request errors inside the intentionally injected process-stop window.",
                "unexpected_availability_errors": "API request errors outside that injected window, including recovery checks.",
                "injected_response_losses": "Successful real upstream finalize responses deliberately dropped before any client response bytes."
            },
            "cases": [{"scenario": s, "status": "not_run", "reason": "not_selected" if s not in selected else "not_started",
                       "checks": [], "metrics": {"injected_downtime_seconds": 0, "injected_response_losses": 0,
                        "availability_errors_during_injected_downtime": 0, "unexpected_availability_errors": 0,
                        "recovery_seconds": 0}} for s in SCENARIOS],
            "limitations": ["Synthetic originals and history are retained; no purge or cleanup deletion.",
             "Graceful worker exit before commit and pending-job restart, not SIGKILL, OOM, or an in-flight crash.",
             "Native MOVE requires a pre-provisioned stable logical gateway and independent native identity/history proof; not implemented by these process hooks.",
             "No browser, Docker/Compose, Peri, 10 GiB, multi-host, or full release acceptance claim."]}


def summarize(report, selected):
    selected_cases = [c for c in report["cases"] if c["scenario"] in selected]
    statuses = [c["status"] for c in selected_cases]
    report["selected_status"] = "fail" if "fail" in statuses else "pass" if statuses and all(s == "pass" for s in statuses) else "partial" if "pass" in statuses else "not_run"
    all_status = [c["status"] for c in report["cases"]]
    report["full_fault_acceptance"] = "fail" if "fail" in all_status else "pass" if all(s == "pass" for s in all_status) else "partial" if "pass" in all_status else "not_run"
    report["status"] = report["full_fault_acceptance"]
    return 1 if report["status"] == "fail" else 0 if report["status"] == "pass" else 3


def run(config, output_path, hooks=None, *, env=None, scenarios=None, check_only=False):
    """Return (exit_code, sanitized_report); only this driver writes its NEW report.

    Output is mandatory and private. Every absent capability is explicit not_run,
    and independent cases continue after a recoverable failure. No raw hook error,
    source response, account, identifier, origin or credential enters the report.
    """
    selected = list(SCENARIOS if scenarios is None else scenarios)
    report = new_report(selected)
    handle = None
    rc = 2
    hooks = Hooks() if hooks is None else hooks
    env = dict(os.environ if env is None else env)
    try:
        require(selected and len(selected) == len(set(selected)) and set(selected) <= set(SCENARIOS), "invalid_scenario_selection")
        require(bool(output_path), "new_private_report_required")
        handle = reserve_report(output_path, env.get("TEST_CREDENTIALS_FILE"))
        validate_config(config)
        hooks.validate()
        report["configuration"] = "valid"
        if check_only:
            report["reason"] = "configuration_only_no_network_or_fault_execution"
            return_code = 3
        else:
            require(env.get("ALLOW_DISPOSABLE_TEST_WRITES") == "yes", "disposable_write_opt_in_required")
            require(env.get("ALLOW_DISPOSABLE_TEST_FAULTS") == "yes", "disposable_fault_opt_in_required")
            require(env.get("ALLOW_DISPOSABLE_TEST_OIDC_CONSENT") == "yes", "disposable_oidc_consent_opt_in_required")
            runnable = []
            for case in report["cases"]:
                name = case["scenario"]
                if name not in selected:
                    continue
                if name == "native_move_response_loss":
                    case["reason"] = "stable_native_gateway_and_history_proof_not_provided"
                elif hooks.snapshot is None:
                    case["reason"] = "read_only_fixture_snapshot_hook_not_provided"
                elif name != "lost_finalize_response" and name.split("_")[0] not in hooks.services:
                    case["reason"] = "test_owned_process_control_not_provided"
                else:
                    runnable.append(case)
            if runnable:
                suite = Suite(config, hooks)
                report["execution"] = "executed"
                suite.owner.login()
                suite.owner.delegate(allow_new_grant=True)
                healthy = True
                for case in runnable:
                    if not healthy:
                        case["reason"] = "prior_service_recovery_unconfirmed"
                        continue
                    suite.case, suite.owner.metrics = case, case["metrics"]
                    try:
                        if case["scenario"] == "lost_finalize_response":
                            suite.lost_finalize_response()
                        else:
                            suite.interrupted_service(case["scenario"].split("_")[0])
                        case["status"] = "pass"
                        case.pop("reason", None)
                    except Exception as exc:
                        # Assertion labels carry useful static specifics; exception
                        # strings are never trusted, including from injected hooks.
                        case["status"], case["reason"] = "fail", failure_code(exc)
                    try:
                        healthy = all(c.running() is True for c in hooks.services.values())
                    except Exception:
                        healthy = False
                    if not healthy:
                        case["status"], case["reason"] = "fail", "service_recovery_unconfirmed"
            return_code = summarize(report, selected)
        rc = return_code
    except DriverError as exc:
        # DriverError raised by our own guards/helpers contains only static codes.
        report["reason"] = failure_code(exc)
        if report["execution"] == "executed":
            report["status"] = report["full_fault_acceptance"] = "fail"
            rc = 1
    except KeyboardInterrupt:
        report["reason"] = "interrupted_outcomes_may_be_unconfirmed"
        report["status"] = "fail" if report["execution"] == "executed" else "not_run"
        report["full_fault_acceptance"] = report["status"]
        rc = 130
    except Exception:
        report["reason"] = "unexpected_driver_error_details_suppressed"
        report["status"] = "fail" if report["execution"] == "executed" else "not_run"
        report["full_fault_acceptance"] = report["status"]
        rc = 1 if report["execution"] == "executed" else 2
    finally:
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        if handle is not None:
            try:
                with handle:
                    json.dump(report, handle, indent=2)
                    handle.write("\n")
            except OSError:
                report["reason"] = "report_write_failed"
                rc = 2
    return rc, report


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check-only", action="store_true")
    parser.add_argument("--output", required=True)
    parser.add_argument("--scenario", default="all")
    args = parser.parse_args(argv)
    env = dict(os.environ if env is None else env)
    try:
        config = Config.from_environment(env, loopback_http=True, need_reader=False)
        selected = list(SCENARIOS) if args.scenario == "all" else args.scenario.split(",")
        rc, report = run(config, args.output, env=env, scenarios=selected, check_only=args.check_only)
    except Exception:
        rc, report = 2, {"suite": "bounded-local-faults", "status": "not_run", "reason": "configuration_invalid"}
    print(json.dumps(report, indent=2))
    return rc


if __name__ == "__main__":
    sys.exit(main())
