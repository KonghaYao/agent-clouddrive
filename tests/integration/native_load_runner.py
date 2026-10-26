#!/usr/bin/env python3
"""Local synthetic native load supervisor. Never launches a browser or Docker.

Run inside the SAME shell that started the disposable PostgreSQL and identity
fixtures. Only this supervisor's Popen children can be interrupted by its hooks.
External/native dependencies are not downloaded by this module.
"""
from __future__ import annotations

import argparse
from contextlib import redirect_stdout
from datetime import datetime, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import threading
import time
import urllib.request

from deployed_api_harness import API, Config, DriverError, require, safe_id

FAULT_SCENARIOS = ("worker_restart", "nats_recovery", "meili_recovery", "lost_finalize_response")
OWNED_SERVICES = ("api", "worker", "maintenance", "meili", "nats")


class RunDeadline:
    """Whole-supervisor budget; cancel before graceful shutdown, never SIGKILL."""
    def __init__(self, started):
        self.started = started
        self.work_deadline = started + 570
        self.expired = threading.Event()
        self.lock = threading.Lock()
        self.services = {}
        self.stop_failures = []
        self.timer = None

    def arm(self, services):
        require(set(services) <= set(OWNED_SERVICES), "deadline_service_not_owned")
        self.services = services
        for service in services.values():
            service.start_guard = self.guard
        self.timer = threading.Timer(max(0, self.work_deadline - time.monotonic()), self.expire)
        self.timer.daemon = True
        self.timer.start()

    def guard(self):
        if time.monotonic() >= self.work_deadline:
            self.expire()
        require(not self.expired.is_set(), "overall_run_deadline_reached")

    def expire(self):
        with self.lock:
            if self.expired.is_set():
                return
            self.expired.set()  # Blocks fault watchdogs/new starts before stopping.
        for name in OWNED_SERVICES:
            if name in self.services:
                try:
                    self.services[name].stop()
                except Exception:
                    self.stop_failures.append(name)

    def pressure_seconds(self, *, reserve_faults):
        self.guard()
        # 570-second work cutoff already reserves final 30 seconds for cleanup.
        available = int(self.work_deadline - time.monotonic() - (120 if reserve_faults else 0))
        require(available >= 60, "insufficient_overall_budget_for_pressure")
        return min(600, available)

    def finish(self, report):
        watchdog_incomplete = False
        if self.timer is not None:
            self.timer.cancel()
            try:
                # cancel() alone does not wait for a callback already stopping
                # children. Join within the 30-second cleanup allowance.
                self.timer.join(timeout=max(0, min(12, self.started + 600 - time.monotonic())))
                watchdog_incomplete = bool(self.timer.is_alive())
            except RuntimeError:
                watchdog_incomplete = True
        elapsed = time.monotonic() - self.started
        report["whole_run_budget"] = {"maximum_seconds": 600, "work_cutoff_seconds": 570,
                                      "elapsed_seconds": round(elapsed, 4),
                                      "watchdog_fired": self.expired.is_set(),
                                      "watchdog_shutdown_incomplete": watchdog_incomplete,
                                      "owned_stop_failures": list(self.stop_failures)}
        if self.expired.is_set() or elapsed >= 570 or self.stop_failures or watchdog_incomplete:
            report.update(status="fail", selected_status="fail", reason="overall_run_deadline_reached")
            return False
        return True


class OwnedService:
    def __init__(self, name, argv, env, log):
        self.name, self.argv, self.env, self.log = name, argv, env, log
        self.process = None
        self.lock = threading.RLock()
        self.start_guard = lambda: None

    def start(self):
        with self.lock:
            self.start_guard()
            if self.running():
                return
            with self.log.open("ab") as output:
                self.process = subprocess.Popen(self.argv, env=self.env, stdout=output,
                                                stderr=subprocess.STDOUT, close_fds=True)

    def running(self):
        return self.process is not None and self.process.poll() is None

    def stop(self):
        with self.lock:
            if self.running():
                self.process.terminate()
                try:
                    self.process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    raise DriverError("owned_service_graceful_stop_timeout") from None


def private_json(path, value):
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL |
                           getattr(os, "O_NOFOLLOW", 0), 0o600), "w") as output:
        json.dump(value, output, indent=2)
        output.write("\n")


def write_nats_config(run_dir):
    """Private per-run native config; never depend on an external wrapper file."""
    path = run_dir / ("nats-load-" + secrets.token_hex(8) + ".conf")
    content = ('listen: "127.0.0.1:54222"\nhttp: "127.0.0.1:58222"\n'
               'jetstream {\n  store_dir: ' + json.dumps(str(run_dir / "jetstream")) + '\n'
               '  max_memory_store: 256MB\n  max_file_store: 1GB\n}\n')
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL |
                           getattr(os, "O_NOFOLLOW", 0), 0o600), "w") as output:
        output.write(content)
    return path


def source_hash(root):
    # Fixed repository-owned helper, never a user-selected module or control hook.
    spec = importlib.util.spec_from_file_location("native_load_release_readiness", root / "scripts/release-readiness.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.source_digest(root)


def new_retention_namespace():
    # A fresh application DB cannot adopt a prior DB's unknown native root.
    # Generate once per supervisor run, then share the same env across app roles.
    return ".clouddrive-retained-native-" + secrets.token_hex(8)


def confirm_source_stability(report, final_hash):
    """A live pass cannot certify changed or unknown executable source."""
    initial = report.get("source_sha256_at_launch")
    known = lambda value: isinstance(value, str) and re.fullmatch(r"[a-f0-9]{64}", value) is not None
    report["source_sha256_at_finish"] = final_hash if known(final_hash) else None
    report["source_changed_during_run"] = initial != final_hash if known(initial) and known(final_hash) else "unknown"
    if report["source_changed_during_run"] is not False:
        report.update(status="fail", selected_status="fail", reason="source_changed_during_run")
        return False
    return True


def wait_health(url, service, timeout=30):
    deadline = time.monotonic() + timeout
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    while time.monotonic() < deadline:
        require(service.running(), "native_child_exited_before_ready")
        try:
            with opener.open(url, timeout=1) as response:
                if response.status == 200:
                    return
        except OSError:
            pass
        time.sleep(.1)
    raise DriverError("native_child_health_timeout")


class Snapshot:
    def __init__(self, psql, env, pg_port):
        self.psql, self.pg_port = psql, pg_port
        self.env = {k: v for k, v in env.items() if not k.startswith("PG")}
        self.env["PGOPTIONS"] = "-c default_transaction_read_only=on -c statement_timeout=1000"

    def __call__(self, file_id, upload_id):
        fid, uid = safe_id(file_id), safe_id(upload_id)
        sql = f"""WITH versions AS (SELECT id FROM file_versions WHERE file_id='{fid}'),
          jobs AS (SELECT id,state,attempt FROM conversion_jobs WHERE source_version_id IN(SELECT id FROM versions)),
          unpublished AS (SELECT attempt FROM outbox WHERE state<>'published' AND
              (aggregate_id='{fid}' OR aggregate_id IN(SELECT id FROM versions) OR aggregate_id IN(SELECT id FROM jobs)
               OR payload->>'file_id'='{fid}' OR payload->>'version_id' IN(SELECT id::text FROM versions)
               OR payload->>'job_id' IN(SELECT id::text FROM jobs)))
          SELECT json_build_object(
            'file_count',(SELECT count(*) FROM files WHERE id='{fid}'),
            'version_count',(SELECT count(*) FROM versions),
            'upload_count',(SELECT count(*) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE o.file_id='{fid}'),
            'committed_reservations',(SELECT count(*) FROM quota_reservations WHERE upload_id='{uid}' AND state='committed'),
            'held_reservations',(SELECT count(*) FROM quota_reservations WHERE upload_id='{uid}' AND state='held'),
            'ready_artifact_count',(SELECT count(*) FROM artifact_sets WHERE source_version_id IN(SELECT id FROM versions) AND published_at IS NOT NULL),
            'unpublished_outbox_count',(SELECT count(*) FROM unpublished),
            'max_unpublished_outbox_attempt',(SELECT COALESCE(max(attempt),0) FROM unpublished),
            'job_states',COALESCE((SELECT json_agg(state ORDER BY id) FROM jobs),'[]'),
            'job_attempts',COALESCE((SELECT json_agg(attempt ORDER BY id) FROM jobs),'[]'))"""
        result = subprocess.run([str(self.psql), "-XAt", "-h", "127.0.0.1", "-p", str(self.pg_port),
                                 "-U", "drive", "-d", "clouddrive", "-v", "ON_ERROR_STOP=1", "-c", sql],
                                env=self.env, capture_output=True, text=True, timeout=2)
        require(result.returncode == 0, "readonly_snapshot_failed")
        try:
            return json.loads(result.stdout)
        except ValueError:
            raise DriverError("readonly_snapshot_invalid") from None


def owned_fault_controls(services):
    from fault_acceptance import ServiceControl
    return {name: ServiceControl(services[name].stop, services[name].start, services[name].running)
            for name in ("worker", "nats", "meili")}


def accept_selected_fault_result(exit_code, report):
    """Accept exact four supported cases without laundering partial into full pass."""
    require(isinstance(report, dict), "fault_report_invalid")
    cases = report.get("cases")
    require(isinstance(cases, list) and len(cases) == 5 and all(isinstance(c, dict) for c in cases),
            "fault_case_scope_mismatch")
    names = [c.get("scenario") for c in cases]
    require(len(set(names)) == 5 and set(names) == set(FAULT_SCENARIOS) | {"native_move_response_loss"},
            "fault_case_scope_mismatch")
    selected = [c for c in cases if c["scenario"] in FAULT_SCENARIOS]
    move = next(c for c in cases if c["scenario"] == "native_move_response_loss")
    require(exit_code == 3 and report.get("execution") == "executed" and report.get("selected_status") == "pass"
            and report.get("full_fault_acceptance") == "partial" and report.get("status") == "partial"
            and len(selected) == 4 and all(c.get("status") == "pass" for c in selected)
            and move.get("status") == "not_run", "fault_scenario_failed_or_scope_misreported")
    return {"exit_code": exit_code, "selected_status": "pass", "full_fault_acceptance": "partial",
            "selected_scenarios": list(FAULT_SCENARIOS), "native_move_response_loss": "not_run"}


def standalone_stress_preflight(config, env):
    require(env.get("ALLOW_DISPOSABLE_TEST_OIDC_CONSENT") == "yes", "explicit_disposable_oidc_consent_required")
    owner = API(config)
    owner.login()
    owner.delegate(allow_new_grant=True)


def stress_arguments(runtime_file, output_file, max_concurrency=16, max_seconds=600):
    require(type(max_concurrency) is int and max_concurrency in (16, 32, 64), "invalid_maximum_concurrency")
    require(type(max_seconds) is int and 60 <= max_seconds <= 600, "invalid_pressure_time_budget")
    return ["--runtime-file", str(runtime_file), "--output", str(output_file), "--allow-disposable-writes",
            "--max-concurrency", str(max_concurrency), "--max-seconds", str(max_seconds)]


def wait_baseline_drain(runtime, result, run_deadline=None):
    """Read-only guarded transition; never waive the stress driver's preflight."""
    import stress_acceptance
    options = stress_acceptance.Options(max_seconds=130)  # Safety reserves 10 seconds.
    options.validate()
    safety = stress_acceptance.Safety(runtime, options, stress_acceptance.Metrics())
    started = time.monotonic()
    deadline = started + 120
    result.update(status="running", samples=[], required_consecutive_zero_samples=3,
                  minimum_sample_interval_seconds=1, maximum_seconds=120)
    stable = 0
    try:
        while stable < 3:
            if run_deadline is not None:
                run_deadline.guard()
            require(time.monotonic() < deadline, "baseline_pipeline_drain_timeout")
            # Includes native root/descendant identity, RSS, memory, disk and PG
            # lock/backlog guards. Do not substitute an unguarded SQL-only poll.
            sample = safety.sample()
            require(sample.get("metrics_complete") is True and sample.get("native_rss_scope") == "configured_pids",
                    "baseline_native_resource_metrics_incomplete")
            result["samples"].append({key: sample[key] for key in (
                "conversion_pending", "index_pending", "outbox_pending", "sampled_rss_bytes",
                "available_ram_bytes", "free_disk_bytes", "active_connections", "waiting_locks")})
            result["samples"][-1]["elapsed_seconds"] = round(time.monotonic() - started, 4)
            empty = sum(sample[key] for key in ("conversion_pending", "index_pending", "outbox_pending")) == 0
            stable = stable + 1 if empty else 0
            require(time.monotonic() <= deadline, "baseline_pipeline_drain_timeout")
            if stable < 3:
                require(deadline - time.monotonic() >= 1, "baseline_pipeline_drain_timeout")
                time.sleep(1)
        result.update(status="pass", consecutive_zero_samples=stable,
                      stable_zero_span_seconds=round(result["samples"][-1]["elapsed_seconds"] -
                                                     result["samples"][-3]["elapsed_seconds"], 4))
    except Exception:
        result["status"] = "fail"
        raise
    finally:
        result["elapsed_seconds"] = round(time.monotonic() - started, 4)


def main(argv=None):
    run_deadline = RunDeadline(time.monotonic())
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("joint", "stress", "fault", "all"), required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--max-concurrency", type=int, choices=(16, 32, 64), default=16)
    args = parser.parse_args(argv)
    require(os.environ.get("ALLOW_DISPOSABLE_TEST_WRITES") == "yes", "synthetic_write_opt_in_required")
    require(os.environ.get("ALLOW_DISPOSABLE_TEST_LOAD") == "yes", "local_load_opt_in_required")
    require(os.environ.get("ALLOW_DISPOSABLE_TEST_OIDC_CONSENT") == "yes", "explicit_disposable_oidc_consent_required")
    if args.mode in ("fault", "all"):
        require(os.environ.get("ALLOW_DISPOSABLE_TEST_FAULTS") == "yes", "disposable_fault_opt_in_required")
    root = Path(__file__).resolve().parents[2]
    runtime = Path(os.environ["NATIVE_RUNTIME"]).resolve()
    run_dir = Path(os.environ["NATIVE_RUN_DIR"]).resolve()
    require(not runtime.is_relative_to(root) and run_dir.is_relative_to(runtime), "unsafe_runtime_location")
    output = Path(args.output_dir).resolve()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    config = Config.from_environment(dict(os.environ), True, args.mode in ("joint", "all"))
    require(config.public == "http://127.0.0.1:58081" and config.issuer == "https://localhost:58443/realms/clouddrive",
            "supervisor_requires_exact_local_fixture")
    env = dict(os.environ, DRIVE_LISTEN="127.0.0.1:58081", DRIVE_MCP_LISTEN="127.0.0.1:58082",
               DRIVE_STORAGE_MODE="local", DRIVE_ARTIFACT_LOCAL_ROOT=str(run_dir / "artifacts"),
               DRIVE_TRASH_RESERVED_DIR=new_retention_namespace(),
               DRIVE_MEILI_INDEX="stress_e2e", DRIVE_JOB_LEASE_SECONDS="12",
               DRIVE_NODE_ID="native-bounded-test", DRIVE_OCIS_INTEGRATION_VERIFIED="true",
               DRIVE_DELEGATION_VERIFIED="true", DRIVE_MIGRATIONS_DIR=str(root / "db/migrations"),
               GOMEMLIMIT="512MiB", GOMAXPROCS="2")
    env.update(TEST_MEILI_URL="http://127.0.0.1:57700", TEST_MEILI_INDEX="stress_e2e")
    pg_library_path = env.get("NATIVE_PG_LIBRARY_PATH") or str(runtime / "postgres/usr/lib/x86_64-linux-gnu")
    require(all(Path(p).is_absolute() and Path(p).is_dir() for p in pg_library_path.split(":")),
            "invalid_postgres_library_path")
    env["LD_LIBRARY_PATH"] = pg_library_path
    nats_config = write_nats_config(run_dir)
    commands = {
        "nats": [str(runtime / "nats-server-v2.15.1-linux-amd64/nats-server"), "-c", str(nats_config)],
        "meili": [str(runtime / "meilisearch"), "--db-path", str(run_dir / "meili-data"), "--http-addr", "127.0.0.1:57700",
                  "--env", "development", "--no-analytics", "--max-indexing-memory", "512MiB", "--max-indexing-threads", "2"],
        **{name: [str(runtime / "bin" / ("drive-" + name))] for name in ("api", "worker", "maintenance")},
    }
    for command in commands.values():
        require(Path(command[0]).is_file() and os.access(command[0], os.X_OK), "native_binary_missing")
    services = {name: OwnedService(name, command, env, run_dir / (name + "-load.log"))
                for name, command in commands.items()}
    report = {"schema_version": 1, "suite": "native-load-supervisor", "status": "not_run",
              "started_at": datetime.now(timezone.utc).isoformat(), "mode": args.mode,
              "scope": "Local synthetic native processes only; not Docker/browser/production capacity.",
              "bounded_resources": {"nats_loopback_only": True, "nats_jetstream_memory_bytes": 256 * 1024 * 1024,
                                    "nats_jetstream_file_bytes": 1024 * 1024 * 1024,
                                    "configured_stress_maximum_concurrency": args.max_concurrency},
              "fixture_isolation": {"native_retention_namespace": "unique_per_supervisor_run",
                                    "shared_by_api_worker_maintenance": True,
                                    "prior_native_roots_adopted_or_modified": False},
              "source_sha256_at_launch": source_hash(root),
              "binary_sha256": {name: hashlib.sha256(Path(cmd[0]).read_bytes()).hexdigest() for name, cmd in commands.items()},
              "scenarios": {}, "cleanup": "pending"}
    rc = 1
    try:
        run_deadline.arm(services)
        run_deadline.guard()
        services["nats"].start(); services["meili"].start()
        wait_health("http://127.0.0.1:58222/healthz", services["nats"])
        wait_health("http://127.0.0.1:57700/health", services["meili"])
        with (run_dir / "migrate-load.log").open("ab") as log:
            migrate = subprocess.run(commands["maintenance"] + ["migrate"], env=env, stdout=log, stderr=log, timeout=30)
        require(migrate.returncode == 0, "native_migration_failed")
        for name in ("api", "worker", "maintenance"):
            services[name].start()
        wait_health(config.public + "/healthz", services["api"])
        native_pids = {name: service.process.pid for name, service in services.items()}
        native_pids.update(keycloak=int(env["NATIVE_KEYCLOAK_PID"]), ocis=int(env["NATIVE_OCIS_PID"]),
                           postgres=int((run_dir / "pgdata/postmaster.pid").read_text().splitlines()[0]))
        runtime_spec = {"schema_version": 1, "disposable": True, "exclusive_owner": True, "public_url": config.public,
                        "oidc_issuer": config.issuer, "synthetic_username": config.credentials["owner"]["username"],
                        "storage_path": str(runtime), "native_pids": native_pids,
                        "postgres": {"psql": str(runtime / "postgres/usr/lib/postgresql/17/bin/psql"),
                                     "library_path": pg_library_path,
                                     "host": "127.0.0.1", "port": 55432, "database": "clouddrive", "user": "drive"}}
        runtime_file = run_dir / ("load-runtime-" + secrets.token_hex(8) + ".json")
        private_json(runtime_file, runtime_spec)
        os.environ.update(env)
        if args.mode in ("joint", "all"):
            run_deadline.guard()
            import deployed_api_acceptance
            with redirect_stdout(io.StringIO()):
                result = deployed_api_acceptance.main(["--scenario", "all", "--allow-disposable-writes", "--allow-loopback-http",
                                                      "--output", str(output / "joint.json")], env=env)
            report["scenarios"]["joint"] = result
            require(result == 0, "joint_scenario_failed")
        if args.mode in ("stress", "all"):
            run_deadline.guard()
            import stress_acceptance
            if args.mode == "stress":
                standalone_stress_preflight(config, env)
            report["baseline_drain"] = {}
            wait_baseline_drain(runtime_spec, report["baseline_drain"], run_deadline)
            pressure_seconds = run_deadline.pressure_seconds(reserve_faults=args.mode == "all")
            report["bounded_resources"]["pressure_maximum_seconds"] = pressure_seconds
            with redirect_stdout(io.StringIO()):
                result = stress_acceptance.main(stress_arguments(runtime_file, output / "stress.json", args.max_concurrency,
                                                                pressure_seconds), env=env)
            report["scenarios"]["stress"] = result
            require(result == 0, "stress_scenario_failed")
        if args.mode in ("fault", "all"):
            run_deadline.guard()
            import fault_acceptance
            controls = owned_fault_controls(services)
            hooks = fault_acceptance.Hooks(services=controls, snapshot=Snapshot(runtime_spec["postgres"]["psql"], env, 55432))
            result, fault_report = fault_acceptance.run(config, str(output / "fault.json"), hooks, env=env,
                                                        scenarios=list(FAULT_SCENARIOS))
            report["scenarios"]["fault"] = accept_selected_fault_result(result, fault_report)
        report["status"] = "partial" if args.mode in ("fault", "all") else "pass"
        report["selected_status"] = "pass"
        rc = 3 if report["status"] == "partial" else 0
    except DriverError as error:
        report.update(status="fail", reason=str(error))
    except Exception:
        report.update(status="fail", reason="supervisor_internal_error")
    finally:
        failed = []
        for name in OWNED_SERVICES:
            try:
                services[name].stop()
            except Exception:
                failed.append(name)
        report["cleanup"] = "graceful_children_stopped" if not failed else "graceful_stop_incomplete"
        if failed:
            report["status"] = report["selected_status"] = "fail"; rc = 1
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
        try:
            final_hash = source_hash(root)
        except Exception:
            final_hash = None
        if not confirm_source_stability(report, final_hash):
            rc = 1
        if not run_deadline.finish(report):
            rc = 1
        private_json(output / "supervisor.json", report)
    print(json.dumps({"status": report["status"], "reason": report.get("reason"), "cleanup": report["cleanup"]}))
    return rc


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except DriverError as error:
        print(json.dumps({"status": "not_run", "reason": str(error)}))
        raise SystemExit(2)
