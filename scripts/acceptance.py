#!/usr/bin/env python3
"""Execute prepared live acceptance drivers, separately from source self-tests.

Planning/checking never launches a browser, grants consent, or marks a case passed.
This entry point does not edit the release matrix or promote its evidence.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[1]
SUITES = {
    "browser": {
        "command": ["node", "web/scripts/browser-acceptance.mjs"],
        "run_args": ["--run"],
        "scope": "Real browser, real OIDC, two users, actual UI file lifecycle and sharing; not Peri or large-file acceptance.",
        "gates": ["A01", "A03", "A13", "A14", "A18", "G09", "A21"],
        "case_ids": ["real_keycloak_owner_login", "real_keycloak_recipient_login", "own_file_list_dom", "read_only_scope_ui_and_backend", "offline_delegation_prerequisite", "upload_first_version_dom", "private_file_isolation_backend", "raster_preview_dom", "overwrite_and_pinned_history_dom_and_backend", "trash_and_restore_dom", "share_explicit_confirmation_dom", "received_share_read_only_dom_and_backend", "share_revocation_dom_and_backend", "anonymous_pdf_link_preview_dom_and_resource_isolation", "anonymous_pdf_link_revocation_dom_and_backend", "fixture_soft_delete_dom"],
    },
    "deployed-api": {
        "command": [sys.executable, "tests/integration/deployed_api_acceptance.py"],
        "run_args": ["--scenario", "all", "--allow-disposable-writes"],
        "scope": "Real deployed API/IdP scenarios; not browser UI, explicit audit-grant coverage, or full release acceptance.",
        "gates": ["A01", "A14", "A15", "A16", "A17"],
        "case_ids": ["retained_versions", "subtree", "restore_conflict", "batch100", "unauthorized"],
    },
    "uppy-10gib": {
        "command": ["node", "web/scripts/large-upload-acceptance.mjs"],
        "run_args": ["--run"],
        "scope": "Exact 10 GiB browser Uppy interruption/resume with deployment and database accounting proof; never a small-file substitute.",
        "gates": ["D02_UPPY_10_GIB", "G06", "A24"],
        "case_ids": ["deployment", "source", "login", "ui_upload", "interruption_resume", "native_session", "commit_once", "quota_once", "download_integrity", "s3_multipart", "processing_state", "receiver_survived"],
    },
}


def suite_environment(name: str, environment: dict[str, str]) -> dict[str, str]:
    """Allow distinct protected credential formats without printing their paths."""
    child = dict(environment)
    alias = {"browser": "TEST_BROWSER_CREDENTIALS_FILE", "deployed-api": "TEST_API_CREDENTIALS_FILE"}.get(name)
    if alias and child.get(alias):
        child["TEST_CREDENTIALS_FILE"] = child[alias]
    return child


def assess_report(report: object, *, checking: bool, returncode: int, expected_suite: str | None = None, expected_ids: list[str] | None = None) -> tuple[str, list[str]]:
    """Fail closed on process/report disagreement, skipped cases, and empty runs."""
    errors = []
    if not isinstance(report, dict):
        return "fail", ["driver result is not an object"]
    if report.get("schema_version") != 1:
        errors.append("unsupported driver result schema")
    if expected_suite is not None and report.get("suite") != expected_suite:
        errors.append("driver reported a different suite; a small smoke cannot substitute for 10 GiB")
    cases = report.get("cases")
    if not isinstance(cases, list) or not cases:
        errors.append("driver must enumerate every requested case")
        cases = []
    ids = set()
    for case in cases:
        if not isinstance(case, dict) or not isinstance(case.get("id"), str) or not case["id"]:
            errors.append("invalid case identity")
            continue
        if case["id"] in ids:
            errors.append("duplicate case identity")
        ids.add(case["id"])
        if case.get("status") not in {"pass", "fail", "not_run"}:
            errors.append("invalid case status")
    if expected_ids is not None and ids != set(expected_ids):
        errors.append("driver must report exactly the requested suite's complete case set")
    status = report.get("status")
    execution = report.get("execution")
    if status not in {"pass", "fail", "not_run"} or execution not in {"executed", "not_run"}:
        errors.append("invalid run status or execution declaration")
    if checking:
        if status != "not_run" or execution != "not_run" or any(c.get("status") != "not_run" for c in cases if isinstance(c, dict)):
            errors.append("a setup check cannot contain execution or passing acceptance cases")
        if returncode not in {2, 3}:
            errors.append("a setup check must exit 2 (missing setup) or 3 (not executed)")
        return ("fail" if errors else "not_run"), errors
    if status == "pass":
        if returncode != 0 or execution != "executed" or not cases or any(c.get("status") != "pass" for c in cases if isinstance(c, dict)):
            errors.append("passing driver must actually execute and pass every case with exit 0")
        if expected_suite == "uppy-10gib" and report.get("accepts_10gib") is not True:
            errors.append("10 GiB suite must explicitly establish exact-size eligibility")
    elif returncode == 0:
        errors.append("non-passing acceptance must not exit 0")
    if execution == "not_run" and any(c.get("status") != "not_run" for c in cases if isinstance(c, dict)):
        errors.append("unexecuted report contains executed cases")
    return ("fail" if errors else status), errors


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["plan", "check", "run"])
    parser.add_argument("--suite", choices=[*SUITES, "all"], default="all")
    parser.add_argument("--output-dir", type=Path, help="A NEW results directory; existing paths are never overwritten.")
    args = parser.parse_args(argv)
    selected = list(SUITES) if args.suite == "all" else [args.suite]
    if args.action == "plan":
        print(json.dumps({"schema_version": 1, "execution": "not_run", "status": "not_run", "suites": [{"id": name, **SUITES[name], "status": "not_run"} for name in selected], "note": "Prepared drivers are not test evidence. No commands executed. See docs/acceptance-drivers.md."}, indent=2))
        return 3
    if args.action == "run" and os.environ.get("ALLOW_DISPOSABLE_TEST_WRITES") != "yes":
        print("NOT RUN: ALLOW_DISPOSABLE_TEST_WRITES=yes is required for dedicated disposable test accounts. No command executed.", file=sys.stderr)
        return 2
    folder = args.output_dir or ROOT / "test-results" / (datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:10])
    try:
        folder.mkdir(parents=True, exist_ok=False, mode=0o700)
    except OSError:
        print("NOT RUN: output directory must be new and writable.", file=sys.stderr)
        return 2
    folder = folder.resolve()
    summary = {"schema_version": 1, "action": args.action, "started_at": datetime.now(timezone.utc).isoformat(), "release_matrix_modified": False, "scope": "Selected driver execution only, not full release readiness", "suites": []}
    for name in selected:
        suite = SUITES[name]
        result_path = folder / f"{name}.json"
        command = [*suite["command"], *(suite["run_args"] if args.action == "run" else ["--check-only"]), "--output", str(result_path)]
        try:
            # Drivers save a deliberately allowlisted JSON result. Arbitrary child
            # logs (including Playwright exceptions/URLs) are not persisted or echoed.
            completed = subprocess.run(command, cwd=ROOT, env=suite_environment(name, dict(os.environ)), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            code = completed.returncode
            report = json.loads(result_path.read_text(encoding="utf-8")) if result_path.is_file() else None
            status, errors = assess_report(report, checking=args.action == "check", returncode=code, expected_suite=name, expected_ids=suite["case_ids"])
        except (OSError, ValueError):
            code, status, errors = 2, "fail", ["driver did not produce a valid sanitized result"]
        summary["suites"].append({"id": name, "status": status, "exit_code": code, "result_file": result_path.name if result_path.is_file() else None, "validation_errors": errors, "scope": suite["scope"], "related_gates_not_promoted": suite["gates"]})
    statuses = [entry["status"] for entry in summary["suites"]]
    summary["status"] = "fail" if "fail" in statuses else "not_run" if "not_run" in statuses else "pass"
    summary["finished_at"] = datetime.now(timezone.utc).isoformat()
    descriptor = os.open(folder / "summary.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        handle.write(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))
    if summary["status"] == "fail":
        return 1
    if args.action == "check" or summary["status"] == "not_run":
        return 2 if any(entry["exit_code"] == 2 for entry in summary["suites"]) else 3
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
