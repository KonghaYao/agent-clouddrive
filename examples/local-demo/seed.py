#!/usr/bin/env python3
"""Seed actual disposable Keycloak/oCIS/app services; never a simulated demo.

Every resource is created through authenticated REST/tus. Idempotency intent is
persisted before each mutation. No credentials, bearer tokens or upload URLs are
written to journals, reports or stdout. Offline tests are NOT live evidence.
"""
from __future__ import annotations
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import stat
import sys
import time
import urllib.parse

from demo_support import (DriverError, Runtime, load_runtime, private_json, private_write,
                          require, safe_id)


def make_pdf(marker):
    """Small standards-compliant searchable one-page PDF, generated at runtime."""
    require(bool(re.fullmatch(r"demo[a-f0-9]{24}", marker)), "invalid_sample_marker")
    text = ("BT /F1 20 Tf 72 760 Td (Agent CloudDrive acceptance PDF) Tj "
            "0 -36 Td /F1 12 Tf (" + marker + " pdfready) Tj "
            "0 -26 Td (Synthetic local sample. Controlled preview uses real raster conversion.) Tj ET").encode("ascii")
    objects = [b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length " + str(len(text)).encode() + b" >>\nstream\n" + text + b"\nendstream"]
    pdf = b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"
    offsets = [0]
    for n, obj in enumerate(objects, 1):
        offsets.append(len(pdf)); pdf += str(n).encode() + b" 0 obj\n" + obj + b"\nendobj\n"
    start = len(pdf)
    pdf += b"xref\n0 6\n0000000000 65535 f \n"
    pdf += b"".join((f"{offset:010d} 00000 n \n").encode() for offset in offsets[1:])
    return pdf + b"trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n" + str(start).encode() + b"\n%%EOF\n"


def sample_sources(marker):
    require(bool(re.fullmatch(r"demo[a-f0-9]{24}", marker)), "invalid_sample_marker")
    return [
        {"step": "notes-v1", "name": "验收说明.md", "local": "验收说明-v1.md", "type": "text/markdown", "previous": None,
         "bytes": (f"# Agent CloudDrive 验收样例\n\n{marker} markdownready\n\n第一版：原件、历史和私有访问。\n").encode()},
        {"step": "notes-v2", "name": "验收说明.md", "local": "验收说明-v2.md", "type": "text/markdown", "previous": "notes-v1",
         "bytes": (f"# Agent CloudDrive 验收样例\n\n{marker} markdownready\n\n第二版：已增加搜索、受控 PDF 预览与人工确认分享步骤。\n\n此文件保留第一版，可以在历史版本中读取。\n").encode()},
        {"step": "text-v1", "name": "搜索样例.txt", "local": "搜索样例.txt", "type": "text/plain", "previous": None,
         "bytes": (f"{marker} textready\n这是只含合成数据的全文搜索样例，不含个人资料。\n可以上传自己的测试文件，等待转换完成后搜索正文。\n").encode()},
        {"step": "pdf-v1", "name": "受控预览.pdf", "local": "受控预览.pdf", "type": "application/pdf", "previous": None,
         "bytes": make_pdf(marker)},
    ]


def schema_journal(doc, binding):
    require(set(doc) == {"schema", "binding", "marker", "folder_name", "steps", "status"}
            and doc.get("schema") == 1 and doc.get("binding") == binding
            and doc.get("status") in ("in_progress", "passed"), "invalid_or_unbound_seed_journal")
    require(isinstance(doc["marker"], str) and re.fullmatch(r"demo[a-f0-9]{24}", doc["marker"])
            and doc["folder_name"] == "验收样例-" + doc["marker"][-8:], "invalid_seed_identity")
    require(isinstance(doc["steps"], dict) and set(doc["steps"]) <= {"directory", "notes-v1", "notes-v2", "text-v1", "pdf-v1"},
            "unexpected_seed_step")
    if doc["status"] == "passed":
        require(set(doc["steps"]) == {"directory", "notes-v1", "notes-v2", "text-v1", "pdf-v1"}
                and all(isinstance(step, dict) and step.get("complete") is True for step in doc["steps"].values()),
                "passed_seed_journal_incomplete")
    keys = set()
    for key, step in doc["steps"].items():
        require(isinstance(step, dict) and set(step) <= {"request", "source_sha256", "operation_id", "file_id", "upload_id", "version_id", "complete"}
                and isinstance(step.get("request"), dict) and type(step.get("complete")) is bool, "invalid_seed_step")
        request = step["request"]
        allowed = {"name", "parent_id", "idempotency_key"} if key == "directory" else {
            "name", "parent_id", "idempotency_key", "size_bytes", "content_type", "file_id", "expected_version_id", "expected_metadata_revision"}
        require(set(request) <= allowed and all(isinstance(request.get(k), str) for k in ("name", "parent_id", "idempotency_key")),
                "invalid_seed_request")
        require(re.fullmatch(r"demo-[a-f0-9]{32}", request["idempotency_key"]) is not None
                and request["idempotency_key"] not in keys, "invalid_seed_idempotency_key")
        keys.add(request["idempotency_key"])
        for field in ("operation_id", "file_id", "upload_id", "version_id"):
            if field in step: safe_id(step[field])
        if key != "directory":
            source = next(s for s in sample_sources(doc["marker"]) if s["step"] == key)
            require(step.get("source_sha256") == hashlib.sha256(source["bytes"]).hexdigest()
                    and request.get("name") == source["name"] and request.get("size_bytes") == len(source["bytes"])
                    and request.get("content_type") == source["type"], "seed_source_or_request_changed")
            directory = doc["steps"].get("directory", {})
            require(directory.get("complete") is True and request["parent_id"] == directory.get("file_id"), "seed_parent_identity_mismatch")
            if source["previous"]:
                previous = doc["steps"].get(source["previous"], {})
                require(previous.get("complete") is True and request.get("file_id") == previous.get("file_id")
                        and request.get("expected_version_id") == previous.get("version_id")
                        and type(request.get("expected_metadata_revision")) is int
                        and request["expected_metadata_revision"] >= 1, "seed_overwrite_identity_mismatch")
                if "file_id" in step: require(step["file_id"] == previous["file_id"], "seed_overwrite_identity_mismatch")
            else:
                require(not any(field in request for field in ("file_id", "expected_version_id", "expected_metadata_revision")), "unexpected_seed_overwrite")
            if step["complete"]: require("file_id" in step and "version_id" in step, "incomplete_seed_record")
        else:
            require(request["name"] == doc["folder_name"] and request["parent_id"] == "", "seed_directory_identity_mismatch")
            if step["complete"]: require("file_id" in step, "incomplete_directory_record")
    return doc


@contextmanager
def journal_lock(root):
    try:
        fd = os.open(root / ".seed.lock", os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
        s = os.fstat(fd)
        require(stat.S_ISREG(s.st_mode) and s.st_uid == os.getuid() and not s.st_mode & 0o077 and s.st_nlink == 1,
                "unsafe_seed_lock")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        raise DriverError("seed_already_running_or_lock_unavailable") from None
    try: yield
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN); os.close(fd)


def published_hit_matches(row, version_id, metadata, marker):
    completeness = row.get("completeness")
    artifact = metadata.get("search_artifact_set_id")
    if not artifact:
        return False
    safe_id(artifact)
    return (row.get("returned_version_id") == version_id and row.get("current_version_id") == version_id
            and metadata.get("current_version_id") == version_id and row.get("artifact_set_id") == artifact
            and row.get("path") == metadata.get("path") and row.get("stale") is False
            and row.get("newer_version_status") == "ready" and isinstance(completeness, dict)
            and completeness.get("status") == "ready" and isinstance(row.get("snippet"), str)
            and marker in row["snippet"])


class Seed:
    def __init__(self, runtime, timeout=180, check_only=False):
        self.runtime, self.check_only, self.timeout = runtime, check_only, timeout
        self.owner, self.reader = runtime.api("owner"), runtime.api("reader")
        self.path = runtime.root / "seed-journal.json"
        binding = hashlib.sha256(json.dumps({"project": runtime.state["project"], "public": runtime.public,
            "issuer": runtime.issuer, "users": [runtime.config.credentials[r]["username"] for r in ("owner", "reader")]}, sort_keys=True).encode()).hexdigest()
        if self.path.exists() or self.path.is_symlink():
            self.journal = schema_journal(private_json(self.path), binding)
        else:
            require(not check_only, "seed_journal_missing_run_start_first")
            marker = "demo" + secrets.token_hex(12)
            self.journal = {"schema": 1, "binding": binding, "marker": marker, "folder_name": "验收样例-" + marker[-8:],
                            "steps": {}, "status": "in_progress"}
            self.save()
        self.sources = sample_sources(self.journal["marker"])

    def save(self):
        schema_journal(self.journal, self.journal["binding"])
        private_write(self.path, self.journal)

    def stat(self, fid): return self.owner.json("GET", "/v1/files/" + safe_id(fid))

    def new_step(self, key, request, source=None):
        step = {"request": request, "complete": False}
        if source is not None: step["source_sha256"] = hashlib.sha256(source).hexdigest()
        self.journal["steps"][key] = step; self.save(); return step

    def mkdir(self):
        step = self.journal["steps"].get("directory")
        if step and step["complete"]:
            meta = self.stat(step["file_id"])
            require(meta.get("name") == self.journal["folder_name"] and meta.get("path") == "/" + self.journal["folder_name"],
                    "demo_directory_changed_no_automatic_repair")
            return step["file_id"]
        require(not self.check_only, "demo_directory_incomplete")
        if step is None:
            step = self.new_step("directory", {"name": self.journal["folder_name"], "parent_id": "", "idempotency_key": "demo-" + secrets.token_hex(16)})
        if not step.get("operation_id"):
            result = self.owner.json("POST", "/v1/directories", step["request"])
            step.update(operation_id=safe_id(result.get("operation_id")), file_id=safe_id(result.get("file_id"))); self.save()
        deadline = time.monotonic() + self.timeout
        while True:
            result = self.owner.json("GET", "/v1/operations/" + step["operation_id"])
            if result.get("state") == "committed": break
            require(result.get("state") in ("prepared", "executing", "remote_confirmed", "outcome_unknown"), "directory_mutation_failed")
            require(time.monotonic() < deadline, "directory_outcome_pending_no_blind_retry"); time.sleep(1)
        step["complete"] = True; self.save(); return step["file_id"]

    def upload(self, source, parent):
        key, payload = source["step"], source["bytes"]
        step = self.journal["steps"].get(key)
        if step and step["complete"]: return
        require(not self.check_only, "demo_upload_incomplete")
        if step is None:
            request = {"name": source["name"], "parent_id": parent, "size_bytes": len(payload),
                       "content_type": source["type"], "idempotency_key": "demo-" + secrets.token_hex(16)}
            if source["previous"]:
                previous = self.journal["steps"][source["previous"]]
                meta = self.stat(previous["file_id"])
                require(meta.get("current_version_id") == previous["version_id"] and meta.get("name") == source["name"]
                        and meta.get("parent_id") == parent, "demo_file_changed_no_automatic_overwrite")
                request.update(file_id=previous["file_id"], expected_version_id=previous["version_id"],
                               expected_metadata_revision=meta["metadata_revision"])
            step = self.new_step(key, request, payload)
        if not step.get("upload_id"):
            result = self.owner.json("POST", "/v1/uploads", step["request"])
            fid = safe_id(result.get("file_id"))
            require(not step["request"].get("file_id") or fid == step["request"]["file_id"], "overwrite_identity_changed")
            step.update(upload_id=safe_id(result.get("id")), file_id=fid); self.save()
        uid, deadline = step["upload_id"], time.monotonic() + self.timeout
        while True:
            # Resolve a lost PATCH/finalize response before sending any more bytes.
            result = self.owner.json("POST", "/v1/uploads/" + uid + "/finalize", {})
            if result.get("status") in ("saved", "committed"):
                require(result.get("file_id") == step["file_id"], "finalize_identity_changed")
                step.update(version_id=safe_id(result.get("version_id")), complete=True); self.save(); return
            require(result.get("status") in ("uploading", "outcome_unknown"), "upload_not_finalized")
            require(time.monotonic() < deadline, "upload_outcome_pending_no_blind_retry")
            if result["status"] == "uploading":
                head = self.owner.request("HEAD", "/v1/tus/" + uid, headers={"Tus-Resumable": "1.0.0"})
                require(head.status == 200 and head.headers.get("Upload-Length") == str(len(payload)), "tus_state_unconfirmed")
                offset = head.headers.get("Upload-Offset", "")
                require(bool(re.fullmatch(r"0|[1-9][0-9]*", offset)) and 0 <= int(offset) <= len(payload), "tus_offset_invalid")
                if int(offset) < len(payload):
                    response = self.owner.request("PATCH", "/v1/tus/" + uid, payload[int(offset):], {
                        "Tus-Resumable": "1.0.0", "Upload-Offset": offset, "Content-Type": "application/offset+octet-stream"})
                    require(response.status == 204 and response.headers.get("Upload-Offset") == str(len(payload)), "tus_patch_outcome_unconfirmed")
            time.sleep(1)

    def write_samples(self):
        directory = self.runtime.root / "samples"
        directory.mkdir(mode=0o700, exist_ok=True)
        st = directory.lstat()
        require(stat.S_ISDIR(st.st_mode) and not stat.S_ISLNK(st.st_mode) and st.st_uid == os.getuid()
                and not st.st_mode & 0o077, "unsafe_samples_directory")
        for source in self.sources:
            path = directory / source["local"]
            if path.exists() or path.is_symlink():
                try:
                    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
                    with os.fdopen(fd, "rb") as f:
                        st = os.fstat(f.fileno())
                        require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077
                                and st.st_nlink == 1 and f.read(8192) == source["bytes"], "local_sample_modified_no_overwrite")
                except OSError: raise DriverError("local_sample_unreadable") from None
            else:
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
                with os.fdopen(fd, "wb") as f: f.write(source["bytes"])

    def verify_sources(self, partial=False):
        steps = self.journal["steps"]
        current = {}
        for source in self.sources:
            step = steps.get(source["step"])
            if not step or not step["complete"]:
                require(partial, "seed_sources_incomplete"); continue
            current[step["file_id"]] = (source, step)
            response = self.owner.request("GET", "/v1/files/" + step["file_id"] + "/content?" + urllib.parse.urlencode({"version_id": step["version_id"]}))
            require(response.status == 200 and response.body == source["bytes"], "seed_historical_source_mismatch")
        for fid, (source, step) in current.items():
            # A pending known overwrite may have committed before its response was recorded.
            pending = [s for s in steps.values() if not s["complete"] and s.get("request", {}).get("file_id") == fid]
            if pending and partial: continue
            meta = self.stat(fid)
            require(meta.get("current_version_id") == step["version_id"] and meta.get("name") == source["name"]
                    and meta.get("parent_id") == steps["directory"]["file_id"], "demo_file_changed_no_automatic_repair")
            response = self.owner.request("GET", "/v1/files/" + fid + "/content")
            require(response.status == 200 and response.body == source["bytes"], "seed_current_source_mismatch")
        return current

    def verify_ready(self, current):
        expected = {fid: step["version_id"] for fid, (_, step) in current.items()}
        deadline = time.monotonic() + self.timeout
        while True:
            result = self.owner.json("POST", "/v1/search", {"query": self.journal["marker"], "limit": 100})
            rows = result.get("items")
            require(isinstance(rows, list) and all(isinstance(row, dict) for row in rows), "invalid_search_response")
            matched = {row.get("file_id") for row in rows}
            ok = matched == set(expected) and not result.get("next_cursor")
            published = {}
            for row in rows:
                fid = row.get("file_id")
                if fid not in expected:
                    ok = False; continue
                metadata = self.stat(fid)
                ok = ok and published_hit_matches(row, expected[fid], metadata, self.journal["marker"])
                published[fid] = metadata.get("search_artifact_set_id")
            if ok: break
            require(time.monotonic() < deadline, "search_or_conversion_not_ready"); time.sleep(1)
        for fid, (source, step) in current.items():
            doc = self.owner.json("GET", "/v1/files/" + fid + "/markdown?" + urllib.parse.urlencode({"version_id": step["version_id"], "artifact_set_id": published[fid]}))
            require(doc.get("version_id") == step["version_id"] and doc.get("artifact_set_id") == published[fid] and self.journal["marker"] in (doc.get("content") or doc.get("markdown") or ""), "markdown_wrong_source_version")
        pdf = self.journal["steps"]["pdf-v1"]
        preview = self.owner.json("GET", "/v1/preview/" + pdf["file_id"])
        require(preview.get("version_id") == pdf["version_id"] and isinstance(preview.get("pages"), list)
                and len(preview["pages"]) == 1 and preview.get("completeness", {}).get("status") == "ready", "pdf_preview_not_ready")
        page = preview["pages"][0]
        require(page.get("page") == 1 and page.get("url") == "/v1/preview/" + pdf["file_id"] + "/pages/1?version_id=" + pdf["version_id"],
                "untrusted_preview_page_url")
        response = self.owner.request("GET", page["url"])
        require(response.status == 200 and response.headers.get("Content-Type", "").split(";")[0] == "image/png"
                and response.body.startswith(b"\x89PNG\r\n\x1a\n") and len(response.body) > 100, "pdf_raster_bytes_missing")
        notes = self.journal["steps"]["notes-v2"]
        versions = self.owner.json("GET", "/v1/files/" + notes["file_id"] + "/versions")
        wanted = {self.journal["steps"][k]["version_id"] for k in ("notes-v1", "notes-v2")}
        require(isinstance(versions.get("items"), list) and {r.get("id") for r in versions["items"]} == wanted
                and not versions.get("next_cursor"), "two_source_versions_not_preserved")
        require(all("conversion_status" in r for r in versions["items"]), "version_conversion_status_missing")

    def run(self, allow_consent):
        owner, reader = self.owner.login(), self.reader.login()
        require(owner["id"] != reader["id"], "synthetic_users_not_distinct")
        status = self.owner.json("GET", "/v1/delegation/status")
        if status.get("enabled") is not True:
            require(not self.check_only and allow_consent, "synthetic_offline_consent_required")
            self.owner.delegate(allow_new_grant=True)
        if not self.check_only: self.write_samples()
        self.verify_sources(partial=True)
        parent = self.mkdir()
        for source in self.sources: self.upload(source, parent)
        current = self.verify_sources()
        self.verify_ready(current)
        result = self.reader.json("POST", "/v1/search", {"query": self.journal["marker"], "limit": 100})
        require(result.get("items") == [] and not result.get("next_cursor"), "reader_private_search_leak")
        for fid in current:
            require(self.reader.request("GET", "/v1/files/" + fid + "/content").status in (403, 404), "reader_private_file_leak")
        self.journal["status"] = "passed"
        if not self.check_only: self.save()
        result = {"schema": 1, "project": self.runtime.state["project"], "status": "passed", "scope": "real-disposable-demo-api", "browser_test": "not_run",
            "checks": {"pkce_profile_and_web_session": "passed", "real_tus_upload": "passed", "immutable_two_versions": "passed",
                       "current_source_search_ready": "passed", "controlled_pdf_png": "passed", "reader_private_isolation": "passed"},
            "folder_name": self.journal["folder_name"], "search_marker": self.journal["marker"], "reader_id": reader["id"],
            "files": [{"name": source["name"], "file_id": fid, "version_id": step["version_id"]} for fid, (source, step) in current.items()],
            "public_share_created": False, "external_vision": "not_configured_not_accepted"}
        private_write(self.runtime.root / "demo-result.json", result)
        return result


def run_seed(runtime: Runtime, allow_consent=False, check_only=False, timeout=180):
    with journal_lock(runtime.root):
        return Seed(runtime, timeout, check_only).run(allow_consent)


def ready(runtime):
    """Light read-only service check; deliberately does not repair/reseed edits."""
    with journal_lock(runtime.root):
        saved = Seed(runtime, check_only=True)
        require(saved.journal["status"] == "passed", "initial_seed_not_verified")
        identities = []
        for role in ("owner", "reader"):
            api = runtime.api(role); api.oauth_login()
            me = api.json("GET", "/v1/me"); identities.append(safe_id(me.get("id")))
        require(identities[0] != identities[1], "synthetic_users_not_distinct")
        result = {"schema": 1, "project": runtime.state["project"], "status": "passed",
                  "scope": "live-auth-api-readiness-only", "sample_contents_rechecked": False}
        private_write(runtime.root / "readiness-result.json", result)
        return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", required=True)
    parser.add_argument("--container-network", action="store_true", help="Only the isolated example Compose backend network")
    parser.add_argument("--allow-synthetic-offline-consent", action="store_true")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--preflight", action="store_true")
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--ready", action="store_true")
    args = parser.parse_args(argv)
    try:
        runtime = load_runtime(args.runtime, args.container_network)
        if args.preflight:
            from preflight import run_preflight
            result = run_preflight(runtime)
        elif args.ready:
            result = ready(runtime)
        else:
            result = run_seed(runtime, args.allow_synthetic_offline_consent, args.check)
        require(result.get("status") == "passed", "demo_verification_not_passed")
        print(json.dumps({"status": "passed", "stage": "preflight" if args.preflight else "ready" if args.ready else "check" if args.check else "seed"}))
        return 0
    except DriverError as e:
        # DriverError codes are static. Never print upstream exception text/body.
        code = str(e) if re.fullmatch(r"[a-z0-9_]{1,120}", str(e)) else "demo_failed_redacted"
        print(json.dumps({"status": "failed", "code": code})); return 2
    except Exception:
        print(json.dumps({"status": "failed", "code": "unexpected_demo_failure_redacted"})); return 2


if __name__ == "__main__":
    raise SystemExit(main())
