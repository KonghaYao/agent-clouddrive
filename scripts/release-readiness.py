#!/usr/bin/env python3
"""Fail-closed release gate. Source self-tests are intentionally a separate command."""
from __future__ import annotations
import argparse
import hashlib
import json
from pathlib import Path
import sys

REQUIRED_IDS = {f"G{i:02d}" for i in range(1, 11)} | {f"A{i:02d}" for i in range(1, 25)} | {"D01_COMPOSE_RUNTIME", "D02_UPPY_10_GIB", "D03_EXTERNAL_VISION"}
STATUSES = {"pass", "partial", "blocked"}
NATIVE_LEVELS = {"native_component", "native_end_to_end"}
NATIVE_REQUIRED = {f"G{i:02d}" for i in range(1, 11)} | {"D01_COMPOSE_RUNTIME", "D02_UPPY_10_GIB", "D03_EXTERNAL_VISION"}
END_TO_END_REQUIRED = {"G02", "G09", "G10", "D01_COMPOSE_RUNTIME", "D02_UPPY_10_GIB", "D03_EXTERNAL_VISION"}
SOURCE_PATHS = ("cmd", "converters", "db", "deploy", "examples", "internal", "scripts", "tests", "web", "Dockerfile", ".dockerignore", "go.mod", "go.sum", "sqlc.yaml", "docs/openapi.json", "docs/mcp-tools.json")
# Mirror the source ZIP exclusion policy in scripts/package.py. In particular,
# native diagnostic cmd/_* programs and runtime credentials are never delivered.
SOURCE_IGNORED_PARTS = {"node_modules", "__pycache__", ".git", "secrets", "generated", "dist", "test-results", "dumps", ".venv", "models", "data"}
SOURCE_IGNORED_SUFFIXES = {".pyc", ".zip", ".db", ".pem", ".key", ".crt", ".secret"}
SOURCE_IGNORED_NAMES = {".env", "credentials.json", "service-credentials.json", "demo-state.json", "seed-journal.json", "preflight-journal.json", "readiness-result.json", "demo-result.json", "native-preflight-result.json", "preflight-result.json", ".deployment-state.json", ".launcher.lock", ".seed.lock"}

def source_digest(root: Path) -> str:
    """Hash the delivered executable source, configuration, tests and contracts.

    Generated caches, build outputs, logs, evidence and prose are not inputs.
    File names and content digests are framed, so renames affect the digest.
    """
    root = root.resolve()
    files = []
    for entry in SOURCE_PATHS:
        target = root / entry
        candidates = target.rglob("*") if target.is_dir() else [target]
        for path in candidates:
            rel = path.relative_to(root)
            if any(part in SOURCE_IGNORED_PARTS or part.startswith(".venv") for part in rel.parts):
                continue
            if any(part.startswith("_") for part in rel.parts[:-1]):
                continue
            if path.is_symlink() or path.name in SOURCE_IGNORED_NAMES or path.suffix in SOURCE_IGNORED_SUFFIXES:
                continue
            if path.is_file():
                if not path.resolve().is_relative_to(root):
                    raise ValueError(f"source file escapes repository: {rel}")
                files.append(path)
    digest = hashlib.sha256()
    for path in sorted(set(files)):
        digest.update(path.relative_to(root).as_posix().encode("utf-8") + b"\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
    return digest.hexdigest()

def inspect(root: Path, manifest: dict) -> dict:
    errors: list[str] = []
    incomplete: list[dict] = []
    if not isinstance(manifest, dict):
        return {"ready": False, "errors": ["manifest must be an object"], "incomplete": [], "counts": {}}
    if manifest.get("schema_version") != 1:
        errors.append("schema_version must be 1")
    registry = manifest.get("evidence")
    gates = manifest.get("gates")
    if not isinstance(registry, dict) or not isinstance(gates, list):
        return {"ready": False, "errors": ["evidence must be an object and gates must be an array"], "incomplete": [], "counts": {}}
    indexed = {}
    for gate in gates:
        if not isinstance(gate, dict) or not isinstance(gate.get("id"), str):
            errors.append("every gate must have a string id"); continue
        gid = gate["id"]
        if gid in indexed:
            errors.append(f"duplicate gate {gid}")
        indexed[gid] = gate
    for gid in sorted(REQUIRED_IDS - indexed.keys()):
        errors.append(f"missing required gate {gid}")
    for gid in sorted(indexed.keys() - REQUIRED_IDS):
        errors.append(f"unexpected gate {gid}; required gate identifiers are fixed")
    checked = {}
    def evidence_ok(eid: str) -> bool:
        if eid in checked:
            return checked[eid]
        record = registry.get(eid)
        if not isinstance(record, dict):
            errors.append(f"unknown evidence ID {eid}"); checked[eid] = False; return False
        rel = record.get("path")
        if not isinstance(rel, str) or not rel or Path(rel).is_absolute():
            errors.append(f"evidence {eid} requires a relative repository path"); checked[eid] = False; return False
        resolved = (root / rel).resolve()
        if not resolved.is_relative_to(root.resolve()) or not resolved.is_file():
            errors.append(f"evidence {eid} missing or outside repository: {rel}"); checked[eid] = False; return False
        raw = resolved.read_bytes()
        if not raw:
            errors.append(f"evidence {eid} is empty: {rel}")
        expected = record.get("sha256")
        if not isinstance(expected, str) or len(expected) != 64 or hashlib.sha256(raw).hexdigest() != expected:
            errors.append(f"evidence {eid} SHA256 mismatch or absent: {rel}")
        if any(not isinstance(record.get(field), str) or not record[field] for field in ("scope", "level")):
            errors.append(f"evidence {eid} must declare its verification level and scope")
        checked[eid] = bool(raw) and expected == hashlib.sha256(raw).hexdigest()
        return checked[eid]
    counts = {status: 0 for status in sorted(STATUSES)}
    for gid, gate in indexed.items():
        status = gate.get("status")
        if status not in STATUSES:
            errors.append(f"gate {gid} has invalid status {status!r}")
        else:
            counts[status] += 1
        for field in ("title", "requirement", "verified_scope", "verification_level"):
            if not isinstance(gate.get(field), str) or not gate[field]:
                errors.append(f"gate {gid} requires {field}")
        refs = gate.get("evidence")
        remaining = gate.get("remaining")
        if not isinstance(refs, list) or not refs or not all(isinstance(x, str) for x in refs):
            errors.append(f"gate {gid} needs nonempty evidence references")
            refs = []
        for eid in refs:
            evidence_ok(eid)
        if not isinstance(remaining, list) or not all(isinstance(x, str) and x for x in remaining):
            errors.append(f"gate {gid} remaining must be an array of nonempty strings")
        if status != "pass":
            incomplete.append({"id": gid, "status": status, "remaining": remaining})
            if not remaining:
                errors.append(f"incomplete gate {gid} must explain remaining work")
        else:
            if remaining:
                errors.append(f"passing gate {gid} still declares remaining work")
            level = gate.get("verification_level")
            if gid in NATIVE_REQUIRED and level not in NATIVE_LEVELS:
                errors.append(f"passing gate {gid} requires native evidence, not {level}")
            if gid in END_TO_END_REQUIRED and level != "native_end_to_end":
                errors.append(f"passing gate {gid} requires actual end-to-end evidence")
            native_refs = [registry[eid] for eid in refs if isinstance(registry.get(eid), dict)]
            if gid in NATIVE_REQUIRED and not any(record.get("level") in NATIVE_LEVELS for record in native_refs):
                errors.append(f"passing gate {gid} has no native evidence reference")
            if gid in END_TO_END_REQUIRED and not any(record.get("level") == "native_end_to_end" for record in native_refs):
                errors.append(f"passing gate {gid} has no end-to-end evidence reference")
    source = manifest.get("source_verification", {})
    if not isinstance(source, dict) or source.get("status") not in STATUSES:
        errors.append("source_verification must declare pass, partial, or blocked status")
    if not isinstance(source, dict) or source.get("status") != "pass":
        incomplete.append({"id": "SOURCE_CURRENT_TREE", "status": source.get("status", "missing") if isinstance(source, dict) else "invalid", "remaining": ["Current delivered source tree must pass the standalone self-test after all fixes."]})
    if isinstance(source, dict):
        if source.get("status") == "pass" and source.get("tree_sha256") != source_digest(root):
            errors.append("passing source_verification has missing or stale tree_sha256; rerun checks after source changes")
        refs = source.get("evidence", [])
        if not isinstance(refs, list) or not refs:
            errors.append("source_verification must reference evidence")
        else:
            for eid in refs:
                if isinstance(eid, str): evidence_ok(eid)
                else: errors.append("source evidence IDs must be strings")
    ready = not errors and not incomplete and REQUIRED_IDS == indexed.keys()
    declared = manifest.get("release_decision")
    if declared not in {"ready", "not_ready"}:
        errors.append("release_decision must be ready or not_ready"); ready = False
    elif ready and declared != "ready":
        errors.append("all gates claim pass but release_decision is not ready; review evidence before changing it"); ready = False
    elif not ready and declared == "ready":
        errors.append("release_decision claims ready despite incomplete or invalid gates")
    return {"ready": ready, "counts": counts, "incomplete": incomplete, "errors": errors}

def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=Path("docs/acceptance.json"))
    parser.add_argument("--json", action="store_true", help="Print machine-readable result; does not weaken gating.")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    path = args.manifest if args.manifest.is_absolute() else root / args.manifest
    try:
        manifest = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(manifest, dict): raise ValueError("manifest must be an object")
        result = inspect(root, manifest)
    except (OSError, ValueError, TypeError) as exc:
        result = {"ready": False, "errors": [str(exc)], "incomplete": [], "counts": {}}
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print("RELEASE READY" if result["ready"] else "NOT RELEASE READY")
        print("Gate counts: " + ", ".join(f"{k}={v}" for k,v in result["counts"].items()))
        for item in result["incomplete"]:
            print(f"  {item['id']}: {item['status']}")
            for remaining in item.get("remaining") or []: print(f"    {remaining}")
        for error in result["errors"]: print(f"  INVALID EVIDENCE/MANIFEST: {error}")
        print("Source test success does not override any incomplete release gate.")
    return 0 if result["ready"] else (2 if result["errors"] else 1)

if __name__ == "__main__":
    sys.exit(main())
