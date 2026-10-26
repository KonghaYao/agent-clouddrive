#!/usr/bin/env python3
"""Bounded, opt-in pressure against a disposable, verified-loopback real stack.

Importing performs no I/O. run(config, runtime, options) is the supervisor entry
point; CLI writes an exclusive private report. No fixture cleanup or blind retry.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timezone
import hashlib
import errno
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import stat
import subprocess
import sys
import threading
import time
import urllib.parse

from deployed_api_harness import API, Config, DriverError, require, safe_id
from deployed_api_acceptance import reserve_report, write_report, search_matches

STAGES = (1, 4, 8, 16)  # Backward-compatible default, never an unbounded generator.
EXTENDED_STAGES = (1, 4, 8, 16, 32, 64)
CONCURRENCY_CHOICES = (16, 32, 64)
WORKLOADS = ("read", "list", "stat", "download", "search", "upload",
             "same_file_conflict", "same_target_conflict", "batch_replay", "index_backlog")
GIB = 1 << 30
MAX_BYTES = 512 << 20
MAX_WORK = 5000
PG_FIELDS = ("conversion_pending", "index_pending", "outbox_pending", "active_connections", "waiting_locks")
# Fixed SQL only. No caller SQL, pg_sleep, EXPLAIN ANALYZE, extensions or mutations.
PG_SQL = """SELECT json_build_object(
 'conversion_pending',(SELECT count(*) FROM conversion_jobs WHERE state IN ('queued','processing')),
 'index_pending',(SELECT count(*) FROM artifact_sets WHERE index_state IN ('pending','enqueued')),
 'outbox_pending',(SELECT count(*) FROM outbox WHERE state <> 'published'),
 'active_connections',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active'),
 'waiting_locks',(SELECT count(*) FROM pg_locks WHERE NOT granted));"""
LIMITATIONS = [
    "Only explicitly configured disposable verified-loopback services and one synthetic owner are tested.",
    "HTTP latency and scenario latency are separate; expected recognized 409s are not unexpected errors.",
    "Process RSS covers deduplicated PPID-derived descendants of named roots plus previously observed reparented children and the driver; snapshots are non-atomic, absent native PIDs means native RSS not_checked.",
    "PostgreSQL backlog is the conversion/index pipeline; it is not a direct Meilisearch internal-queue measurement.",
    "Fixture reservation includes 8x payload or 64KiB per upload attempt, plus directory allowance; opaque service WAL/log/storage amplification is not measured as fixture bytes.",
    "No browser, Docker, production, 10 GiB upload, fault injection or release-readiness claim.",
    "Failed reports and all synthetic fixtures are retained; no automatic purge, cleanup or mutation replay after uncertain outcomes.",
]


def private_json(path: str) -> dict:
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "r", encoding="utf-8") as handle:
            st = os.fstat(handle.fileno())
            require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077,
                    "runtime_requires_owned_private_regular_file")
            require(st.st_size <= 65536, "runtime_too_large")
            data = json.load(handle)
        require(isinstance(data, dict), "invalid_runtime_contract")
        return data
    except DriverError:
        raise
    except (OSError, ValueError, UnicodeError):
        raise DriverError("runtime_unreadable_or_invalid") from None


def literal_loopback(url: str, *, resolve_localhost=False):
    try:
        p = urllib.parse.urlsplit(url)
        require(p.scheme in ("http", "https") and not p.username and not p.password
                and not p.query and not p.fragment, "literal_loopback_endpoint_required")
        if p.hostname == "localhost":
            # Only this exact reserved name is permitted for existing fixture
            # certificate/issuer consistency. Actual execution verifies every
            # resolved address before any authentication or load.
            if resolve_localhost:
                addresses = socket.getaddrinfo("localhost", p.port or (443 if p.scheme == "https" else 80),
                                               type=socket.SOCK_STREAM)
                require(bool(addresses) and all(ipaddress.ip_address(item[4][0]).is_loopback for item in addresses),
                        "localhost_did_not_resolve_exclusively_loopback")
            return
        require(ipaddress.ip_address(p.hostname).is_loopback, "literal_loopback_endpoint_required")
    except (ValueError, TypeError, OSError):
        raise DriverError("literal_loopback_endpoint_required") from None


def validate_runtime(runtime: dict, config: Config) -> dict:
    require(runtime.get("schema_version") == 1 and runtime.get("disposable") is True,
            "disposable_runtime_attestation_required")
    for key, actual in (("public_url", config.public), ("oidc_issuer", config.issuer)):
        require(runtime.get(key) == actual, "runtime_endpoint_mismatch")
        literal_loopback(actual)
    if config.meili_url:
        literal_loopback(config.meili_url)
    username = config.credentials["owner"]["username"]
    require(runtime.get("synthetic_username") == username and runtime.get("exclusive_owner") is True,
            "exclusive_synthetic_owner_attestation_required")
    require(isinstance(runtime.get("storage_path"), str) and Path(runtime["storage_path"]).is_dir(),
            "storage_path_required")
    pids = runtime.get("native_pids", {})
    require(isinstance(pids, dict) and len(pids) <= 32 and
            all(re.fullmatch(r"[a-z][a-z0-9_]{0,31}", key) and type(pid) is int and pid > 1
                for key, pid in pids.items()) and len(set(pids.values())) == len(pids), "invalid_native_pids")
    pg = runtime.get("postgres")
    require(isinstance(pg, dict), "postgres_metrics_required")
    try:
        require(ipaddress.ip_address(pg.get("host")).is_loopback, "postgres_loopback_required")
    except (ValueError, TypeError):
        raise DriverError("postgres_loopback_required") from None
    require(type(pg.get("port")) is int and 1 <= pg["port"] <= 65535, "invalid_postgres_port")
    require(all(isinstance(pg.get(key), str) and re.fullmatch(r"[A-Za-z0-9_][A-Za-z0-9_-]{0,62}", pg[key])
                for key in ("database", "user")), "invalid_postgres_identifier")
    require(isinstance(pg.get("psql"), str) and Path(pg["psql"]).is_absolute()
            and Path(pg["psql"]).is_file() and os.access(pg["psql"], os.X_OK), "psql_executable_required")
    if pg.get("library_path"):
        require(isinstance(pg["library_path"], str) and all(Path(x).is_absolute() and Path(x).is_dir()
                for x in pg["library_path"].split(":")), "invalid_postgres_library_path")
    if pg.get("passfile"):
        try:
            st = os.stat(pg["passfile"], follow_symlinks=False)
            require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077,
                    "postgres_passfile_requires_private_regular_file")
        except OSError:
            raise DriverError("postgres_passfile_unreadable") from None
    return runtime


@dataclass(frozen=True)
class Options:
    stage_seconds: float = 10.0
    max_seconds: float = 600.0
    payload_bytes: int = 1024
    max_new_bytes: int = MAX_BYTES
    max_work_items: int = MAX_WORK
    maximum_concurrency: int = 16

    @property
    def schedule(self):
        require(type(self.maximum_concurrency) is int and self.maximum_concurrency in CONCURRENCY_CHOICES,
                "invalid_maximum_concurrency")
        return tuple(level for level in EXTENDED_STAGES if level <= self.maximum_concurrency)

    def validate(self):
        require(type(self.maximum_concurrency) is int and self.maximum_concurrency in CONCURRENCY_CHOICES,
                "invalid_maximum_concurrency")
        require(math.isfinite(self.stage_seconds) and 1 <= self.stage_seconds <= 30, "invalid_stage_seconds")
        require(math.isfinite(self.max_seconds) and 60 <= self.max_seconds <= 600, "invalid_total_seconds")
        require(type(self.payload_bytes) is int and 256 <= self.payload_bytes <= 65536, "invalid_payload_size")
        require(type(self.max_new_bytes) is int and self.payload_bytes <= self.max_new_bytes <= MAX_BYTES,
                "invalid_fixture_byte_cap")
        require(type(self.max_work_items) is int and 1 <= self.max_work_items <= MAX_WORK, "invalid_work_cap")


def percentile(values, quantile):
    """Nearest-rank percentile; empty evidence is null, never an invented zero."""
    require(0 < quantile <= 1, "invalid_quantile")
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * quantile) - 1)]


def summarize(samples, seconds):
    latency = [sample["seconds"] for sample in samples]
    unexpected = sum(sample["unexpected"] for sample in samples)
    expected409 = sum(sample["expected_409"] for sample in samples)
    return {"count": len(samples), "throughput_per_second": len(samples) / seconds if seconds > 0 else None,
            "p50_seconds": percentile(latency, .50), "p95_seconds": percentile(latency, .95),
            "p99_seconds": percentile(latency, .99), "unexpected_errors": unexpected,
            "unexpected_error_ratio": unexpected / len(samples) if samples else None,
            "expected_409": expected409,
            "expected_409_ratio": expected409 / len(samples) if samples else None}


def threshold_reason(http_summary=None, scenario_summary=None, sample=None):
    for summary in (http_summary, scenario_summary):
        if summary and summary["count"]:
            if summary["unexpected_error_ratio"] > .10:
                return "unexpected_error_ratio_exceeded"
            if summary["p99_seconds"] > 10:
                return "p99_latency_exceeded"
    if sample:
        require(all(type(sample.get(k)) is int and sample[k] >= 0 for k in PG_FIELDS), "required_postgres_metrics_missing")
        require(type(sample.get("available_ram_bytes")) is int and type(sample.get("free_disk_bytes")) is int,
                "required_resource_metrics_missing")
        if sample["available_ram_bytes"] < 2 * GIB:
            return "available_ram_below_2gib"
        if sample["free_disk_bytes"] < 5 * GIB:
            return "free_disk_below_5gib"
        if sample.get("sampled_rss_bytes") is not None and sample["sampled_rss_bytes"] > 4 * GIB:
            return "sampled_rss_exceeded_4gib"
        if sum(sample[key] for key in ("conversion_pending", "index_pending", "outbox_pending")) > MAX_WORK:
            return "queued_work_exceeded_5000"
    return None


def summarize_resource_window(samples, start_elapsed_seconds, end_elapsed_seconds):
    """Summarize existing full-fidelity samples in an inclusive elapsed window.

    These are sampled extrema, not continuous system maxima. Missing evidence
    stays null/not_observed; it must never become a zero-resource pass.
    """
    require(all(type(value) in (int, float) and math.isfinite(value) for value in
                (start_elapsed_seconds, end_elapsed_seconds))
            and 0 <= start_elapsed_seconds <= end_elapsed_seconds, "invalid_resource_window")
    selected = []
    for sample in samples:
        elapsed = sample.get("elapsed_seconds")
        require(type(elapsed) in (int, float) and math.isfinite(elapsed) and elapsed >= 0,
                "invalid_resource_sample_time")
        if start_elapsed_seconds <= elapsed <= end_elapsed_seconds:
            require(sample.get("metrics_complete") is True and
                    all(type(sample.get(key)) is int and sample[key] >= 0 for key in
                        ("sampled_rss_bytes", "available_ram_bytes", "free_disk_bytes") + PG_FIELDS),
                    "incomplete_stage_resource_sample")
            selected.append(sample)
    result = {"status": "observed" if selected else "not_observed", "sample_count": len(selected),
              "sampling_basis": "existing_point_in_time_observations_not_continuous",
              "scope": "full_stage_including_races_load_audit_and_drain",
              "window_start_elapsed_seconds": start_elapsed_seconds,
              "window_end_elapsed_seconds": end_elapsed_seconds,
              "rss_scopes": sorted({sample.get("native_rss_scope", "not_checked") for sample in selected})}
    for key in ("sampled_rss_bytes",) + PG_FIELDS:
        result["peak_" + key] = max((sample[key] for sample in selected), default=None)
    for key in ("available_ram_bytes", "free_disk_bytes"):
        result["min_" + key] = min((sample[key] for sample in selected), default=None)
    result["peak_total_pending_work"] = max((sum(sample[key] for key in
        ("conversion_pending", "index_pending", "outbox_pending")) for sample in selected), default=None)
    return result


def counter_threshold(count, unexpected, over_ten_seconds):
    """Exact integer equivalent of error>10% or nearest-rank P99>10s.

    The P99 rank is ceil(99*n/100). It exceeds ten seconds exactly when more
    than n-rank observations exceed ten. No sampling or approximate quantile.
    """
    require(all(type(value) is int for value in (count, unexpected, over_ten_seconds))
            and count >= 0 and 0 <= unexpected <= count and 0 <= over_ten_seconds <= count,
            "invalid_guard_counters")
    if count == 0:
        return None
    if unexpected * 10 > count:
        return "unexpected_error_ratio_exceeded"
    rank = (99 * count + 99) // 100
    if over_ten_seconds > count - rank:
        return "p99_latency_exceeded"
    return None


def safe_reason(error):
    value = str(error) if isinstance(error, DriverError) else "internal_driver_failure"
    return value if re.fullmatch(r"[a-z0-9_]{1,160}", value) else "driver_validation_failure"


class Metrics:
    def __init__(self):
        self.lock = threading.Lock()
        self.http = []
        self.scenarios = []
        self.resources = []
        self.resource_errors = []
        self.guard_counters = {}

    def record(self, kind, stage, workload, seconds, unexpected=False, expected409=False, status=None, phase="unspecified"):
        sample = {"stage": stage, "workload": workload, "phase": phase, "seconds": seconds,
                  "unexpected": bool(unexpected), "expected_409": bool(expected409)}
        if status is not None:
            sample["status"] = status
        with self.lock:
            getattr(self, kind).append(sample)
            for scope in {None, workload}:
                counters = self.guard_counters.setdefault((kind, stage, scope), [0, 0, 0])
                counters[0] += 1
                counters[1] += int(bool(unexpected))
                counters[2] += int(seconds > 10)

    def guard_reason(self, stage):
        # Constant work per observation: two counters × eleven bounded scopes.
        # Full original samples remain untouched for exact final quantiles.
        with self.lock:
            for workload in (None,) + WORKLOADS:
                for kind in ("http", "scenarios"):
                    reason = counter_threshold(*self.guard_counters.get((kind, stage, workload), (0, 0, 0)))
                    if reason:
                        return reason
        return None

    def summary(self, kind, stage=None, workload=None, seconds=1, phase=None):
        with self.lock:
            values = [x for x in getattr(self, kind) if (stage is None or x["stage"] == stage)
                      and (workload is None or x["workload"] == workload) and (phase is None or x["phase"] == phase)]
        result = summarize(values, seconds)
        result["throughput_unit"] = "api_http_requests_per_second" if kind == "http" else "scenarios_per_second"
        result["throughput_denominator_seconds"] = seconds
        if kind == "http":
            result["status_counts"] = {str(status): sum(x.get("status") == status for x in values)
                                       for status in sorted({x.get("status", 0) for x in values})}
        return result


def parse_proc_stat(raw, expected_pid, page_size):
    """Read only non-secret proc stat identity/PPID/RSS, never argv or environ."""
    try:
        opening, closing = raw.index("("), raw.rindex(")")
        pid = int(raw[:opening].strip())
        fields = raw[closing + 1:].split()
        require(pid == expected_pid and len(fields) >= 22, "invalid_process_stat")
        ppid, started, rss = int(fields[1]), int(fields[19]), int(fields[21])
        require(ppid >= 0 and started >= 0 and rss >= 0 and len(fields[0]) == 1, "invalid_process_stat")
        name = raw[opening + 1:closing]
        name = name if re.fullmatch(r"[A-Za-z0-9_.-]{1,32}", name) else "process"
        return {"pid": pid, "ppid": ppid, "state": fields[0], "start_ticks": str(started),
                "rss_bytes": rss * page_size, "process_name": name}
    except DriverError:
        raise
    except (ValueError, IndexError, TypeError):
        raise DriverError("invalid_process_stat") from None


def process_snapshot(proc_root=Path("/proc")):
    """Complete all enumerated PID stat reads, or fail; disappearance is explicit.

    Linux-compatible runtimes may omit task/TID/children entirely. PPID is the
    primary enumeration contract, not an optional fallback guessed from empties.
    This is an honest non-atomic sampling snapshot, not continuous monitoring.
    """
    try:
        paths = sorted((p for p in proc_root.iterdir() if p.name.isdecimal()), key=lambda p: int(p.name))
        require(bool(paths), "process_snapshot_empty")
        snapshot, exited = {}, []
        for path in paths:
            pid = int(path.name)
            try:
                snapshot[pid] = parse_proc_stat((path / "stat").read_text(), pid, os.sysconf("SC_PAGE_SIZE"))
            except OSError as error:
                if error.errno in (errno.ENOENT, errno.ESRCH) and not path.exists():
                    exited.append({"pid": pid, "reason": "exited_during_proc_enumeration"})
                    continue
                raise DriverError("process_snapshot_incomplete") from None
        return snapshot, exited
    except DriverError:
        raise
    except OSError:
        raise DriverError("process_snapshot_unavailable") from None


def select_process_trees(snapshot, roots):
    """Pure PPID transitive closure; overlaps are retained for explicit dedup."""
    children = {}
    for pid, record in snapshot.items():
        children.setdefault(record["ppid"], set()).add(pid)
    result = {}
    for label, root in roots.items():
        require(root in snapshot, "named_native_process_not_live")
        require(snapshot[root]["state"] not in ("Z", "X"), "named_native_process_not_live")
        selected, todo = set(), [root]
        while todo:
            pid = todo.pop()
            require(pid not in selected, "invalid_process_parent_cycle")
            selected.add(pid)
            todo.extend(children.get(pid, ()))
        result[label] = selected
    return result


def sample_process_trees(roots, root_starts, tracked=None, proc_root=Path("/proc")):
    snapshot, exits = process_snapshot(proc_root)
    groups = select_process_trees(snapshot, roots)
    memberships = {}
    for label, selected in groups.items():
        root = snapshot[roots[label]]
        require(label not in root_starts or root_starts[label] == root["start_ticks"], "named_pid_identity_changed")
        root_starts[label] = root["start_ticks"]
        for pid in selected:
            memberships.setdefault(pid, set()).add(label)
    # Once observed, keep a live child in scope even if it is reparented. PID
    # reuse never preserves attribution: its kernel start identity must match.
    for pid, prior in (tracked or {}).items():
        current = snapshot.get(pid)
        if current is not None and current["start_ticks"] == prior["start_ticks"] and current["state"] not in ("Z", "X"):
            memberships.setdefault(pid, set()).update(prior["roots"])
        elif pid not in memberships:
            exits.append({"pid": pid, "reason": "previously_sampled_descendant_exited"})
    rss, identities, retained = {}, {}, {}
    for pid, labels in memberships.items():
        record = snapshot[pid]
        if record["state"] in ("Z", "X"):
            exits.append({"pid": pid, "reason": "exited_descendant", "roots": sorted(labels)})
            continue
        rss[str(pid)] = record["rss_bytes"]
        identity = {"root": sorted(labels)[0], "roots": sorted(labels), "start_ticks": record["start_ticks"],
                    "parent_pid": record["ppid"], "process_name": record["process_name"]}
        identities[str(pid)] = identity
        retained[pid] = identity
    driver = snapshot.get(os.getpid())
    require(driver is not None and driver["state"] not in ("Z", "X"), "driver_process_metrics_missing")
    rss[str(os.getpid())] = driver["rss_bytes"]
    if str(os.getpid()) not in identities:
        identities[str(os.getpid())] = {"root": "pressure_driver", "roots": ["pressure_driver"],
            "start_ticks": driver["start_ticks"], "parent_pid": driver["ppid"], "process_name": driver["process_name"]}
    proof = {"method": "proc_stat_ppid_snapshot", "enumeration_complete": True, "atomic_snapshot": False,
             "enumerated_live_and_zombie_pids": len(snapshot), "unique_native_processes": len(retained),
             "roots": {label: {"root_pid": root, "live_processes": sum(label in item["roots"] for item in retained.values()),
                       "live_descendants": sum(pid != root and label in item["roots"] for pid, item in retained.items())}
                       for label, root in roots.items()}}
    return rss, identities, exits, proof, retained


class Safety:
    def __init__(self, runtime, options, metrics):
        self.runtime, self.options, self.metrics = runtime, options, metrics
        self.started = time.monotonic()
        # Leave time for already-running 5-second requests and report persistence.
        self.deadline = self.started + options.max_seconds - 10
        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.reason = None
        self.bytes_reserved = 0
        self.fixture_allocation_reserved = 0
        self.work_reserved = 0
        self.pid_starts = {}
        self.tracked_processes = {}
        self.sampling_lock = threading.Lock()

    def abort(self, reason):
        with self.lock:
            if self.reason is None:
                self.reason = reason
        self.stop.set()

    def guard(self):
        if time.monotonic() >= self.deadline:
            self.abort("total_time_limit_reached")
        require(not self.stop.is_set(), self.reason or "stopped")

    def reserve(self, *, size=0, work=0):
        self.guard()
        with self.lock:
            # Conservative source/retention/derived-data allowance, not a claim
            # about opaque dependency WAL or log amplification.
            allocation = max(size * 8, 65536) if size else work * 4096
            require(self.fixture_allocation_reserved + allocation <= self.options.max_new_bytes,
                    "fixture_byte_cap_reached")
            require(self.bytes_reserved + size <= self.options.max_new_bytes, "fixture_byte_cap_reached")
            require(self.work_reserved + work <= self.options.max_work_items, "work_item_cap_reached")
            self.bytes_reserved += size
            self.fixture_allocation_reserved += allocation
            self.work_reserved += work

    def check_results(self, stage):
        reason = self.metrics.guard_reason(stage)
        if reason:
            self.abort(reason)

    def sample(self):
        with self.sampling_lock:
            try:
                return self._sample()
            except Exception as error:
                with self.metrics.lock:
                    self.metrics.resource_errors.append({"reason": safe_reason(error),
                        "elapsed_seconds": time.monotonic() - self.started})
                raise

    def _sample(self):
        self.guard()
        try:
            mem = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
            available = int(mem["MemAvailable"].strip().split()[0]) * 1024
            cgroup = Path("/sys/fs/cgroup")
            if (cgroup / "memory.max").is_file():
                maximum = (cgroup / "memory.max").read_text().strip()
                if maximum != "max":
                    available = min(available, max(0, int(maximum) - int((cgroup / "memory.current").read_text())))
            rss, identities, exited, process_proof, self.tracked_processes = sample_process_trees(
                self.runtime.get("native_pids", {}), self.pid_starts, self.tracked_processes)
            pg = self.runtime["postgres"]
            env = {"PATH": "/usr/bin:/bin", "LC_ALL": "C", "PGCONNECT_TIMEOUT": "2",
                   "PGOPTIONS": "-c default_transaction_read_only=on -c statement_timeout=2000 -c lock_timeout=1000",
                   "PGPASSFILE": pg.get("passfile", "/dev/null"), "PGAPPNAME": "clouddrive_bounded_metrics"}
            if pg.get("library_path"):
                env["LD_LIBRARY_PATH"] = pg["library_path"]
            result = subprocess.run([pg["psql"], "--no-psqlrc", "--no-password", "--tuples-only", "--no-align",
                                     "--host", pg["host"], "--port", str(pg["port"]), "--username", pg["user"],
                                     "--dbname", pg["database"], "--command", PG_SQL],
                                    capture_output=True, text=True, timeout=3, env=env, check=False)
            require(result.returncode == 0 and len(result.stdout) <= 8192, "postgres_metrics_unavailable")
            values = json.loads(result.stdout)
            require(isinstance(values, dict) and all(type(values.get(k)) is int and values[k] >= 0 for k in PG_FIELDS),
                    "invalid_postgres_metrics")
            sample = {"elapsed_seconds": time.monotonic() - self.started, "available_ram_bytes": available,
                      "free_disk_bytes": shutil.disk_usage(self.runtime["storage_path"]).free,
                      "sampled_rss_bytes": sum(rss.values()), "rss_by_pid": rss,
                      "process_identities": identities, "exited_descendants": exited, "process_tree_proof": process_proof,
                      "metrics_complete": True,
                      "native_rss_scope": "configured_pids" if self.runtime.get("native_pids") else "not_checked",
                      **{k: values[k] for k in PG_FIELDS}}
        except DriverError:
            raise
        except (OSError, ValueError, KeyError, IndexError, subprocess.SubprocessError):
            raise DriverError("resource_metrics_unavailable") from None
        with self.metrics.lock:
            self.metrics.resources.append(sample)
        reason = threshold_reason(sample=sample)
        if reason:
            self.abort(reason)
            raise DriverError(reason)
        return sample


class Session:
    """Per-thread transport, real PKCE bearer, serialized refresh on its issuer."""
    def __init__(self, suite, stage, workload):
        self.suite, self.stage, self.workload = suite, stage, workload
        self.api = API(suite.config)

    def request(self, method, path, body=None, headers=None, conflict_codes=()):
        s = self.suite
        s.safety.guard()
        with s.auth_lock:
            if time.monotonic() + 45 >= s.auth.expires:
                require(bool(s.auth.refresh), "session_expired_reauthorization_required")
                s.auth._tokens(s.auth.http.form(s.auth.token_endpoint, {
                    "grant_type": "refresh_token", "client_id": s.config.client_id, "refresh_token": s.auth.refresh}))
            self.api.access, self.api.expires = s.auth.access, s.auth.expires
            self.api.refresh = ""  # Never independently rotate one shared refresh token.
        started, unexpected, expected409, status = time.monotonic(), False, False, 0
        try:
            response = self.api.request(method, path, body, headers)
            status = response.status
            if status == 409 and conflict_codes:
                data = response.json()
                expected409 = data.get("error", {}).get("code") in conflict_codes
            unexpected = status not in (200, 204) and not expected409
            require(not unexpected, "unexpected_api_status_" + str(status))
            return response
        except Exception:
            unexpected = True
            raise
        finally:
            s.metrics.record("http", self.stage, self.workload, time.monotonic() - started,
                             unexpected, expected409, status, phase=s.phase)
            s.safety.check_results(self.stage)

    def json(self, method, path, body=None):
        response = self.request(method, path, body)
        require(response.status == 200, "expected_json_http_200")
        return response.json()


class Suite:
    def __init__(self, config, runtime, options, report):
        self.config, self.runtime, self.options, self.report = config, runtime, options, report
        self.metrics = Metrics()
        self.safety = Safety(runtime, options, self.metrics)
        self.auth = API(config)
        self.auth_lock = threading.Lock()
        self.fixture_lock = threading.Lock()
        self.prefix = "stress" + secrets.token_hex(8)
        self.fixtures = {}
        self.directories = {}
        self.counter = 0
        self.backlog_observed = False
        self.monitor_done = threading.Event()
        self.local = threading.local()
        self.phase = "setup"

    @staticmethod
    def key():
        return "stress-" + secrets.token_hex(16)

    def session(self, stage, workload):
        if not hasattr(self.local, "session"):
            self.local.session = Session(self, stage, workload)
        client = self.local.session
        client.stage, client.workload = stage, workload
        return client

    def unique(self):
        with self.fixture_lock:
            self.counter += 1
            return self.counter

    def fixture_payload(self, marker):
        text = (marker + " synthetic bounded original text\n").encode()
        return text + b"x" * (self.options.payload_bytes - len(text))

    def stat(self, client, fid):
        return client.json("GET", "/v1/files/" + safe_id(fid))

    def quota(self, client):
        q = client.json("GET", "/v1/me").get("quota", {})
        require(all(type(q.get(k)) is int and q[k] >= 0 for k in ("used_bytes", "reserved_bytes")),
                "invalid_quota_response")
        return q["used_bytes"], q["reserved_bytes"]

    def commit(self, client, result):
        oid, fid = safe_id(result.get("operation_id")), safe_id(result.get("file_id"))
        deadline = min(time.monotonic() + 10, self.safety.deadline)
        while result.get("state") in ("prepared", "executing", "remote_confirmed", "outcome_unknown"):
            require(time.monotonic() < deadline, "mutation_pending_no_blind_retry")
            time.sleep(.1)
            result = client.json("GET", "/v1/operations/" + oid)
            require(result.get("file_id") == fid, "operation_file_identity_changed")
        require(result.get("state") == "committed", "mutation_not_committed")
        return fid

    def mkdir(self, client, name, parent=""):
        body = {"name": name, "parent_id": parent, "idempotency_key": self.key()}
        if parent:
            body["expected_parent_revision"] = self.stat(client, parent)["metadata_revision"]
        self.safety.reserve(work=1)
        fid = self.commit(client, client.json("POST", "/v1/directories", body))
        self.directories[fid] = {"name": name, "parent_id": parent}
        return fid

    def begin_body(self, name, parent, payload, previous=None):
        body = {"name": name, "parent_id": parent, "content_type": "text/plain", "size_bytes": len(payload),
                "idempotency_key": self.key()}
        if previous:
            body.update(file_id=previous["id"], expected_version_id=previous["current_version_id"],
                        expected_metadata_revision=previous["metadata_revision"])
        return body

    def finish_upload(self, client, begin, name, parent, payload, marker):
        uid, fid = safe_id(begin.get("id")), safe_id(begin.get("file_id"))
        response = client.request("PATCH", "/v1/tus/" + uid, payload,
                                  {"Tus-Resumable": "1.0.0", "Upload-Offset": "0",
                                   "Content-Type": "application/offset+octet-stream"})
        require(response.status == 204 and response.headers.get("Upload-Offset") == str(len(payload)), "tus_offset_or_status_mismatch")
        # Finalize once. An unknown outcome is failure, not permission to replay mutation.
        result = client.json("POST", "/v1/uploads/" + uid + "/finalize", {})
        require(result.get("status") in ("saved", "committed") and result.get("file_id") == fid,
                "upload_finalize_not_confirmed")
        vid = safe_id(result.get("version_id"))
        with self.fixture_lock:
            fixture = self.fixtures.setdefault(fid, {"name": name, "parent_id": parent, "versions": []})
            require(fixture["name"] == name and fixture["parent_id"] == parent, "upload_identity_collision")
            require(vid not in {v["id"] for v in fixture["versions"]}, "duplicate_committed_version")
            fixture["versions"].append({"id": vid, "payload": payload, "marker": marker})
        if client.workload == "index_backlog":
            sample = self.safety.sample()
            self.backlog_observed |= sample["conversion_pending"] + sample["index_pending"] > 0
        self.verify_file(client, fid, history=True)
        return fid

    def upload(self, client, name, parent, marker, previous=None):
        marker = "stressmarker" + secrets.token_hex(16)
        payload = self.fixture_payload(marker)
        self.safety.reserve(size=len(payload), work=1)
        begin = client.json("POST", "/v1/uploads", self.begin_body(name, parent, payload, previous))
        if previous:
            require(begin.get("file_id") == previous["id"], "overwrite_changed_file_identity")
        return self.finish_upload(client, begin, name, parent, payload, marker)

    def verify_file(self, client, fid, history=False):
        fixture = self.fixtures[fid]
        current = fixture["versions"][-1]
        meta = self.stat(client, fid)
        require(meta.get("id") == fid and meta.get("name") == fixture["name"]
                and meta.get("parent_id", "") == fixture["parent_id"] and meta.get("kind") == "file"
                and meta.get("lifecycle") == "active" and meta.get("current_version_id") == current["id"]
                and meta.get("size_bytes") == len(current["payload"]), "file_metadata_invariant_failed")
        result = client.request("GET", "/v1/files/" + fid + "/content")
        require(result.status == 200 and result.body == current["payload"], "current_content_mismatch")
        if history:
            rows = client.json("GET", "/v1/files/" + fid + "/versions").get("items")
            require(isinstance(rows, list) and len(rows) == len(fixture["versions"])
                    and {r.get("id") for r in rows} == {v["id"] for v in fixture["versions"]}
                    and {r.get("version_seq") for r in rows} == set(range(1, len(rows) + 1)),
                    "exact_version_history_mismatch")
            byid = {r["id"]: r for r in rows}
            for version in fixture["versions"]:
                row = byid[version["id"]]
                require(row.get("file_id") == fid and row.get("size") == len(version["payload"])
                        and row.get("sha256") == hashlib.sha256(version["payload"]).hexdigest(), "version_metadata_mismatch")
                result = client.request("GET", "/v1/files/" + fid + "/content?version_id=" + version["id"])
                require(result.status == 200 and result.body == version["payload"], "retained_content_mismatch")
        return meta

    def listing(self, client, parent):
        rows, cursor = [], ""
        for _ in range(51):
            query = {"parent_id": parent, "limit": 100}
            if cursor:
                query["cursor"] = cursor
            result = client.json("GET", "/v1/files?" + urllib.parse.urlencode(query))
            page = result.get("items")
            require(isinstance(page, list) and all(isinstance(r, dict) for r in page), "invalid_file_listing")
            rows.extend(page)
            next_cursor = result.get("next_cursor")
            if not next_cursor:
                require(len(rows) == len({r.get("id") for r in rows}), "duplicate_list_identity")
                return rows
            require(isinstance(next_cursor, str) and next_cursor != cursor, "invalid_list_cursor")
            cursor = next_cursor
        raise DriverError("fixture_listing_exceeded_cap")

    def verify_listing(self, client, parent):
        expected = {fid: value for fid, value in {**self.directories, **self.fixtures}.items()
                    if value["parent_id"] == parent}
        rows = self.listing(client, parent)
        require({r.get("id") for r in rows} == set(expected), "exact_fixture_listing_mismatch")
        for row in rows:
            wanted = expected[row["id"]]
            require(row.get("name") == wanted["name"] and row.get("parent_id", "") == parent,
                    "listed_metadata_mismatch")

    def expected_search(self, fid):
        fixture = self.fixtures[fid]
        version = fixture["versions"][-1]
        parts, parent = [fixture["name"]], fixture["parent_id"]
        while parent:
            directory = self.directories[parent]
            parts.append(directory["name"])
            parent = directory["parent_id"]
        expected = {"version_id": version["id"], "path": "/" + "/".join(reversed(parts)), "marker": version["marker"]}
        if version.get("artifact_id"):
            expected["artifact_set_id"] = version["artifact_id"]
        return {fid: expected}

    def search(self, client, fid, wait=False):
        expected = self.expected_search(fid)
        marker = expected[fid]["marker"]
        deadline = min(self.safety.deadline, time.monotonic() + 90)
        while True:
            result = client.json("POST", "/v1/search", {"query": '"' + marker + '"', "limit": 100})
            if search_matches(result, expected):
                metadata = self.stat(client, fid)
                artifacts = {row["artifact_set_id"] for row in result["items"]}
                require(len(artifacts) == 1 and metadata.get("current_version_id") == expected[fid]["version_id"]
                        and metadata.get("search_artifact_set_id") in artifacts,
                        "search_artifact_not_current_published_metadata")
                version = self.fixtures[fid]["versions"][-1]
                if version.get("artifact_id"):
                    require(version["artifact_id"] == metadata["search_artifact_set_id"], "search_artifact_identity_changed")
                version["artifact_id"] = metadata["search_artifact_set_id"]
                return
            require(wait and time.monotonic() < deadline, "exact_search_visibility_mismatch")
            time.sleep(.2)

    @staticmethod
    def batch_identity(result):
        rows = result.get("items")
        require(result.get("state") == "completed" and isinstance(rows, list) and len(rows) == 4
                and result.get("failed") == [] and result.get("pending") == [] and result.get("succeeded") == rows,
                "batch_completion_mismatch")
        output = {}
        for row in rows:
            require(row.get("status") == "succeeded", "batch_item_not_succeeded")
            value = row.get("result", {}).get("value", {})
            require(value.get("state") == "committed", "batch_item_not_committed")
            output[row.get("item_id")] = (safe_id(value.get("operation_id")), safe_id(value.get("file_id")))
        require(set(output) == {"item-" + str(n) for n in range(4)} and len({x[1] for x in output.values()}) == 4,
                "batch_duplicate_identity")
        return safe_id(result.get("batch_id")), output

    def setup(self):
        self.safety.sample()
        self.auth.login()
        self.auth.delegate(allow_new_grant=False)
        client = self.session(0, "setup")
        self.baseline = self.quota(client)
        require(self.baseline[1] == 0, "preexisting_quota_reservation")
        sample = self.safety.sample()
        require(sample["conversion_pending"] + sample["index_pending"] + sample["outbox_pending"] == 0,
                "preexisting_pipeline_backlog")
        self.root = self.mkdir(client, self.prefix)
        self.read_root = self.mkdir(client, "read", self.root)
        self.write_root = self.mkdir(client, "write", self.root)
        self.batch_root = self.mkdir(client, "batch", self.root)
        self.seed = self.upload(client, "seed.txt", self.read_root, self.prefix + "seed")
        self.conflict_file = self.upload(client, "same-file.txt", self.write_root, self.prefix + "conflict0")
        revision = self.stat(client, self.batch_root)["metadata_revision"]
        self.batch = {"idempotency_key": self.key(), "items": [
            {"item_id": "item-" + str(n), "command": "mkdir", "depends_on": [], "idempotency_key": self.key(),
             "input": {"parent_id": self.batch_root, "expected_parent_revision": revision, "name": "entry-" + str(n)}}
            for n in range(4)]}
        self.safety.reserve(work=4)
        self.batch_ids = self.batch_identity(client.json("POST", "/v1/batches", self.batch))
        for n in range(4):
            fid = self.batch_ids[1]["item-" + str(n)][1]
            self.directories[fid] = {"name": "entry-" + str(n), "parent_id": self.batch_root}
        self.search(client, self.seed, wait=True)
        self.check_quota(client)

    def check_quota(self, client):
        expected = self.baseline[0] + sum(len(v["versions"][-1]["payload"]) for v in self.fixtures.values())
        require(self.quota(client) == (expected, 0), "exact_latest_only_quota_or_reservation_mismatch")

    def invoke(self, stage, workload, operation):
        started, failed = time.monotonic(), False
        try:
            operation(self.session(stage, workload))
        except Exception as error:
            failed = True
            self.safety.abort(safe_reason(error))
        finally:
            self.metrics.record("scenarios", stage, workload, time.monotonic() - started, failed, phase=self.phase)
            self.safety.check_results(stage)

    def race(self, stage, concurrency, overwrite):
        workload = "same_file_conflict" if overwrite else "same_target_conflict"
        client = self.session(stage, workload)
        previous = self.stat(client, self.conflict_file) if overwrite else None
        name = previous["name"] if overwrite else "race-target-" + str(stage) + ".txt"
        marker = "stressmarker" + secrets.token_hex(16)
        payload = self.fixture_payload(marker)
        count, winners, conflicts = max(2, concurrency), [], []
        lock = threading.Lock()
        barrier = threading.Barrier(concurrency) if concurrency > 1 else None
        def attempt(client):
            self.safety.reserve(size=len(payload), work=1)
            body = self.begin_body(name, self.write_root, payload, previous)
            if barrier:
                barrier.wait(timeout=5)
            response = client.request("POST", "/v1/uploads", body,
                                      conflict_codes=("VERSION_CONFLICT", "OPERATION_PENDING") if overwrite
                                      else ("PATH_CONFLICT", "OPERATION_PENDING"))
            if response.status == 409:
                with lock:
                    conflicts.append(True)
                return
            begin = response.json()
            if overwrite:
                require(begin.get("file_id") == self.conflict_file, "race_overwrite_changed_identity")
            fid = self.finish_upload(client, begin, name, self.write_root, payload, marker)
            with lock:
                winners.append(fid)
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            futures = [pool.submit(self.invoke, stage, workload, attempt) for _ in range(count)]
            for future in futures:
                future.result()
        self.safety.guard()
        require(len(winners) == 1 and len(conflicts) == count - 1, "race_requires_exactly_one_winner")
        self.verify_file(client, winners[0], history=True)
        self.verify_listing(client, self.write_root)
        self.check_quota(client)
        if overwrite:
            self.stale_file = previous
        else:
            self.target_name = name
        return {"attempts": count, "winners": len(winners), "expected_409": len(conflicts), "status": "pass"}

    def workload(self, client, name):
        if name == "read":
            self.verify_file(client, self.seed, history=True)
        elif name == "list":
            self.verify_listing(client, self.read_root)
        elif name == "stat":
            current = self.fixtures[self.seed]["versions"][-1]
            meta = self.stat(client, self.seed)
            require(meta.get("id") == self.seed and meta.get("current_version_id") == current["id"]
                    and meta.get("size_bytes") == len(current["payload"]), "stat_identity_version_size_mismatch")
        elif name == "download":
            response = client.request("GET", "/v1/files/" + self.seed + "/content")
            require(response.status == 200 and response.body == self.fixtures[self.seed]["versions"][-1]["payload"],
                    "download_bytes_mismatch")
        elif name == "search":
            self.search(client, self.seed)
        elif name in ("upload", "index_backlog"):
            number = self.unique()
            self.upload(client, "original-" + str(number) + ".txt", self.write_root,
                        self.prefix + "original" + str(number))
        elif name in ("same_file_conflict", "same_target_conflict"):
            previous = self.stale_file if name == "same_file_conflict" else None
            filename = previous["name"] if previous else self.target_name
            payload = self.fixture_payload(self.prefix + "mustnotcommit")
            body = self.begin_body(filename, self.write_root, payload, previous)
            self.safety.reserve(size=len(payload), work=1)
            response = client.request("POST", "/v1/uploads", body,
                                      conflict_codes=("VERSION_CONFLICT",) if previous else ("PATH_CONFLICT",))
            require(response.status == 409, "expected_conflict_missing")
        elif name == "batch_replay":
            actual = self.batch_identity(client.json("POST", "/v1/batches", self.batch))
            require(actual == self.batch_ids, "batch_replay_changed_identity")
            self.verify_listing(client, self.batch_root)

    def monitor(self):
        while not self.monitor_done.wait(.5):
            try:
                sample = self.safety.sample()
                self.backlog_observed |= sample["conversion_pending"] + sample["index_pending"] > 0
            except Exception as error:
                self.safety.abort(safe_reason(error))
                return

    def stage(self, number, concurrency):
        data = {"stage": number, "concurrency": concurrency, "status": "running", "workloads": {}, "races": {}}
        self.report["stages"].append(data)
        started, timed_start, timed_end = time.monotonic(), None, None
        data["start_elapsed_seconds"] = started - self.safety.started
        previous_versions = {fid: f["versions"][-1]["id"] for fid, f in self.fixtures.items()}
        try:
            self.phase = "race"
            data["races"]["same_file"] = self.race(number, concurrency, True)
            data["races"]["same_target"] = self.race(number, concurrency, False)
            self.phase = "timed"
            timed_start, allocation, allocation_lock = time.monotonic(), [0], threading.Lock()
            deadline = min(timed_start + self.options.stage_seconds, self.safety.deadline)
            def worker():
                while time.monotonic() < deadline and not self.safety.stop.is_set():
                    with allocation_lock:
                        sequence = allocation[0]
                        allocation[0] += 1
                    name = WORKLOADS[sequence % len(WORKLOADS)]
                    self.invoke(number, name, lambda client: self.workload(client, name))
            with ThreadPoolExecutor(max_workers=concurrency) as pool:
                for result in [pool.submit(worker) for _ in range(concurrency)]:
                    result.result()
            timed_end = time.monotonic()
            self.safety.guard()
            self.phase = "audit"
            client = self.session(number, "stage_audit")
            self.verify_file(client, self.conflict_file, history=True)
            self.verify_listing(client, self.write_root)
            self.verify_listing(client, self.batch_root)
            self.check_quota(client)
            for workload in WORKLOADS:
                require(self.metrics.summary("scenarios", number, workload, phase="timed")["count"] > 0,
                        "stage_workload_evidence_missing")
            changed = [fid for fid, f in self.fixtures.items() if previous_versions.get(fid) != f["versions"][-1]["id"]]
            for fid in changed:
                self.search(client, fid, wait=True)
            data["drain"] = self.drain_pipeline()
            data["drain"]["exact_current_search_files_verified"] = len(changed)
            data["status"] = "pass"
        finally:
            finished = time.monotonic()
            data["elapsed_seconds"] = finished - started
            data["end_elapsed_seconds"] = finished - self.safety.started
            data["timed_start_elapsed_seconds"] = timed_start - self.safety.started if timed_start else None
            data["timed_end_elapsed_seconds"] = (timed_end or finished) - self.safety.started if timed_start else None
            data["timed_load_seconds"] = (timed_end or finished) - timed_start if timed_start else 0
            data["http"] = self.metrics.summary("http", number, seconds=data["elapsed_seconds"])
            data["scenarios"] = self.metrics.summary("scenarios", number, seconds=data["elapsed_seconds"])
            data["http"]["throughput_basis"] = "full_stage_wall_clock_including_races_audit_and_drain"
            data["scenarios"]["throughput_basis"] = "full_stage_wall_clock_including_races_audit_and_drain"
            data["timed_http"] = self.metrics.summary("http", number, seconds=data["timed_load_seconds"], phase="timed")
            data["timed_http"]["throughput_basis"] = "timed_load_wall_clock_including_inflight_completion"
            data["timed_workloads"] = {}
            for workload in WORKLOADS:
                data["timed_workloads"][workload] = {
                    "http": self.metrics.summary("http", number, workload, data["timed_load_seconds"], phase="timed"),
                    "scenarios": self.metrics.summary("scenarios", number, workload, data["timed_load_seconds"], phase="timed")}
                data["workloads"][workload] = {
                    "http": self.metrics.summary("http", number, workload, data["elapsed_seconds"]),
                    "scenarios": self.metrics.summary("scenarios", number, workload, data["elapsed_seconds"])}
            resource_error = None
            try:
                with self.metrics.lock:
                    resource_samples = list(self.metrics.resources)
                data["resources"] = summarize_resource_window(resource_samples, data["start_elapsed_seconds"],
                                                              data["end_elapsed_seconds"])
                if data["resources"]["sample_count"] == 0:
                    resource_error = "stage_resource_evidence_missing"
            except DriverError as error:
                resource_error = safe_reason(error)
                data["resources"] = {"status": "not_verified", "reason": resource_error, "sample_count": None}
            if data["status"] == "pass" and resource_error:
                data["status"] = "fail"
                raise DriverError(resource_error)
            if data["status"] != "pass":
                data["status"] = "stopped" if self.safety.stop.is_set() else "fail"

    def drain_pipeline(self):
        deadline = min(self.safety.deadline, time.monotonic() + 90)
        stable, samples = 0, []
        while stable < 3:
            sample = self.safety.sample()
            samples.append(sample["elapsed_seconds"])
            empty = sample["conversion_pending"] + sample["index_pending"] + sample["outbox_pending"] == 0
            stable = stable + 1 if empty else 0
            require(time.monotonic() < deadline, "pipeline_backlog_not_drained")
            if stable < 3:
                time.sleep(.5)
        return {"status": "pass", "consecutive_zero_samples": stable,
                "sample_span_seconds": samples[-1] - samples[-3]}

    def final_audit(self):
        self.phase = "final_audit"
        client = self.session(len(self.options.schedule) + 1, "final_audit")
        for fid in self.fixtures:
            self.verify_file(client, fid, history=True)
            self.search(client, fid, wait=True)
        for parent in (self.root, self.read_root, self.write_root, self.batch_root):
            self.verify_listing(client, parent)
        self.check_quota(client)
        self.report["final_drain"] = self.drain_pipeline()
        require(self.backlog_observed, "pipeline_backlog_not_observed")
        self.report["invariants"] = {"status": "pass", "files": len(self.fixtures),
            "directories": len(self.directories), "versions": sum(len(f["versions"]) for f in self.fixtures.values()),
            "exact_original_and_retained_bytes": True, "exact_version_ids_sequences_hashes": True,
            "latest_only_quota_and_zero_reservations": True, "no_duplicate_paths_or_batch_items": True,
            "exact_public_search_file_version_artifact_path": True,
            "pipeline_backlog_observed_and_drained": True}


def new_report():
    return {"schema_version": 1, "suite": "bounded-deployed-api-pressure", "execution": "not_run", "status": "not_run",
            "started_at": datetime.now(timezone.utc).isoformat(), "stages": [], "invariants": {"status": "not_run"},
            "limitations": LIMITATIONS}


def run(config: Config, runtime: dict, options: Options | None = None, *, report=None,
        allow_disposable_writes=False, oidc_consent=False):
    """Real supervisor entry. Call only after explicit disposable-write/consent approval.

    Returns a truthful pass/fail/stopped report and never prints credentials. The
    supervisor must persist it to a new private file, including failed outcomes.
    """
    require(allow_disposable_writes is True, "explicit_disposable_write_optin_required")
    require(oidc_consent is True, "explicit_disposable_oidc_consent_required")
    options = options or Options()
    options.validate()
    validate_runtime(runtime, config)
    for endpoint in (config.public, config.issuer, config.meili_url):
        if endpoint:
            literal_loopback(endpoint, resolve_localhost=True)
    require(0 < config.timeout <= 5, "http_timeout_must_not_exceed_5_seconds")
    report = report if report is not None else new_report()
    report.update(execution="started", status="running", requested_concurrency_schedule=list(options.schedule),
                  maximum_requested_concurrency=options.maximum_concurrency)
    suite = Suite(config, runtime, options, report)
    monitor = None
    try:
        suite.setup()
        monitor = threading.Thread(target=suite.monitor, daemon=True)
        monitor.start()
        for n, concurrency in enumerate(options.schedule, 1):
            suite.stage(n, concurrency)
        suite.final_audit()
        require(len(report["stages"]) == len(options.schedule) and all(x["status"] == "pass" for x in report["stages"]),
                "missing_stage_evidence")
        report["status"] = "pass"
    except Exception as error:
        report["reason"] = suite.safety.reason or safe_reason(error)
        report["status"] = "stopped" if suite.safety.stop.is_set() else "fail"
        report["invariants"]["status"] = "not_verified"
        for stage in report["stages"]:
            if stage["status"] == "running":
                stage["status"] = report["status"]
    finally:
        suite.monitor_done.set()
        if monitor:
            monitor.join(timeout=4)
        if monitor and monitor.is_alive():
            suite.safety.abort("resource_monitor_not_stopped")
        if suite.safety.stop.is_set():
            report["status"] = "stopped"
            report["reason"] = suite.safety.reason or "stopped"
            report["invariants"]["status"] = "not_verified"
        report["execution"] = "finished"
        elapsed = time.monotonic() - suite.safety.started
        report["elapsed_seconds"] = elapsed
        report["budgets"] = {"new_original_bytes_reserved_upper_bound": suite.safety.bytes_reserved,
                             "max_new_original_bytes": options.max_new_bytes,
                             "conservative_fixture_allocation_reserved_bytes": suite.safety.fixture_allocation_reserved,
                             "new_work_items_reserved": suite.safety.work_reserved,
                             "max_new_work_items": options.max_work_items,
                             "max_seconds": options.max_seconds, "maximum_scheduled_concurrency": options.maximum_concurrency}
        report["http"] = suite.metrics.summary("http", seconds=elapsed)
        report["scenarios"] = suite.metrics.summary("scenarios", seconds=elapsed)
        report["http"]["throughput_basis"] = "full_run_wall_clock_including_setup_races_audit_and_drain"
        report["scenarios"]["throughput_basis"] = "full_run_wall_clock_including_setup_races_audit_and_drain"
        timed_seconds = sum(stage.get("timed_load_seconds", 0) for stage in report["stages"])
        report["timed_http"] = suite.metrics.summary("http", seconds=timed_seconds, phase="timed")
        report["timed_http"]["throughput_basis"] = "sum_of_timed_load_wall_clock_windows_including_inflight_completion"
        report["executed_concurrency_schedule"] = [stage.get("concurrency") for stage in report["stages"]]
        executed_levels = [level for level in report["executed_concurrency_schedule"] if type(level) is int]
        report["maximum_executed_concurrency"] = max(executed_levels, default=0)
        report["resource_samples"] = suite.metrics.resources
        # A monitor observation may finish concurrently with a stage boundary.
        # Recompute from the retained final set after monitor join, so the
        # reported windows and raw samples remain exactly reproducible.
        for stage in report["stages"]:
            if "start_elapsed_seconds" in stage and "end_elapsed_seconds" in stage:
                try:
                    stage["resources"] = summarize_resource_window(suite.metrics.resources,
                        stage["start_elapsed_seconds"], stage["end_elapsed_seconds"])
                    require(stage["resources"]["sample_count"] > 0 or stage["status"] != "pass",
                            "stage_resource_evidence_missing")
                except DriverError as error:
                    stage["resources"] = {"status": "not_verified", "reason": safe_reason(error), "sample_count": None}
                    if stage["status"] == "pass":
                        stage["status"] = "fail"
                    if report["status"] == "pass":
                        report["status"] = "fail"
                        report["reason"] = safe_reason(error)
                        report["invariants"]["status"] = "not_verified"
        report["resource_sampling_errors"] = suite.metrics.resource_errors
        report["backlog_observed"] = suite.backlog_observed
        report["fixture_manifest"] = [{"file_id": fid, "versions": [{"id": v["id"], "size": len(v["payload"]),
                         "sha256": hashlib.sha256(v["payload"]).hexdigest()} for v in fixture["versions"]]}
                         for fid, fixture in suite.fixtures.items()]
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
    return report


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime-file", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--check-only", action="store_true")
    parser.add_argument("--allow-disposable-writes", action="store_true")
    parser.add_argument("--stage-seconds", type=float, default=10)
    parser.add_argument("--max-seconds", type=float, default=600)
    parser.add_argument("--payload-bytes", type=int, default=1024)
    parser.add_argument("--max-concurrency", type=int, choices=CONCURRENCY_CHOICES, default=16)
    args = parser.parse_args(argv)
    env = os.environ if env is None else env
    report, reserved = new_report(), None
    try:
        options = Options(args.stage_seconds, args.max_seconds, args.payload_bytes, maximum_concurrency=args.max_concurrency)
        options.validate()
        config = Config.from_environment(env, loopback_http=True, need_reader=False)
        config.timeout = 5
        runtime = validate_runtime(private_json(args.runtime_file), config)
        require(Path(args.output).resolve() != Path(args.runtime_file).resolve(), "report_cannot_replace_runtime")
        reserved = reserve_report(args.output, env.get("TEST_CREDENTIALS_FILE"))
        report["configuration"] = "valid"
        report["requested_concurrency_schedule"] = list(options.schedule)
        report["maximum_requested_concurrency"] = options.maximum_concurrency
        if args.check_only:
            result = 3
        else:
            require(args.allow_disposable_writes, "explicit_disposable_write_optin_required")
            require(env.get("ALLOW_DISPOSABLE_TEST_OIDC_CONSENT") == "yes", "explicit_disposable_oidc_consent_required")
            run(config, runtime, options, report=report, allow_disposable_writes=True, oidc_consent=True)
            result = 0 if report["status"] == "pass" else 1
    except Exception as error:
        report["reason"] = safe_reason(error)
        report["configuration"] = "invalid" if report["execution"] == "not_run" else report.get("configuration", "valid")
        result = 2 if report["execution"] == "not_run" else 1
    if reserved is not None:
        write_report(report, None, None, reserved)
    else:
        print(json.dumps(report, indent=2))
    return result


if __name__ == "__main__":
    sys.exit(main())
