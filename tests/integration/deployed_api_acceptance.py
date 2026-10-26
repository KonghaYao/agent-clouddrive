#!/usr/bin/env python3
"""Prepared, opt-in deployed REST acceptance. Never promotes the release matrix.

--check-only makes no network requests and exits 3 for valid configuration or 2
for invalid configuration. A real run exits 0 only when every selected case passes.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import secrets
import sys
import time
import urllib.parse

from deployed_api_harness import API, Config, DriverError, Transport, require, safe_id

CASE_IDS = {"retained_versions": "A16", "subtree": "A15", "restore_conflict": "A14",
            "batch100": "A17", "unauthorized": "AUTH_ROUTES"}
# Each is an actual bearer-protected REST route in internal/transport/http.go.
# Public capability routes, OAuth callbacks, metadata, healthz and OPTIONS are
# intentionally not bearer routes. MCP has its own transport and is not claimed.
AUTH_ROUTES = (
    ("POST", "/v1/session"), ("GET", "/v1/admin/jobs"), ("GET", "/v1/admin/feedback"),
    ("GET", "/v1/delegation/status"), ("GET", "/v1/me"), ("GET", "/v1/files"),
    ("GET", "/v1/files/:id"), ("POST", "/v1/directories"), ("PATCH", "/v1/files/:id/location"),
    ("POST", "/v1/uploads"), ("POST", "/v1/uploads/:id/finalize"), ("GET", "/v1/operations/:id"),
    ("GET", "/v1/files/:id/versions"), ("GET", "/v1/files/:id/markdown"),
    ("POST", "/v1/files/:id/conversions"), ("POST", "/v1/search"), ("DELETE", "/v1/files/:id"),
    ("GET", "/v1/trash"), ("POST", "/v1/trash/:id/restore"), ("POST", "/v1/batches"),
    ("POST", "/v1/share-requests"), ("GET", "/v1/share-requests"), ("GET", "/v1/share-requests/:id"),
    ("POST", "/v1/share-requests/:id/approve"), ("GET", "/v1/shares"),
    ("POST", "/v1/shares/:id/revoke"), ("GET", "/v1/shared-with-me"), ("POST", "/v1/feedback"),
    ("POST", "/v1/delegation/start"), ("GET", "/v1/files/:id/content"),
    ("GET", "/v1/audit/files/:id/versions"), ("GET", "/v1/audit/files/:id/content"),
    ("GET", "/v1/audit/files/:id/markdown"), ("GET", "/v1/audit/files/:id/artifacts/:artifact/assets/:asset"),
    ("GET", "/v1/artifacts/:id/assets/:asset"), ("HEAD", "/v1/tus/:id"), ("PATCH", "/v1/tus/:id"),
    ("GET", "/v1/preview/:id"), ("GET", "/v1/preview/:id/pages/:page"),
)
LIMITATIONS = [
    "Prepared driver execution is not a historical acceptance result or a release-readiness decision.",
    "Uses real configured Keycloak PKCE and API only; no browser, Peri, Compose provenance or 10 GiB claim.",
    "No operator-provisioned audit grants: owner history is read before, during and after deletion; separate auditor-grant routes are not exercised.",
    "Subtree raw index source_path is checked only when TEST_MEILI_URL is explicitly configured; otherwise raw_index_scope is not_checked.",
    "Authorization case checks every bearer-protected REST route without credentials, not every authenticated scope/role permutation or MCP.",
    "Does not disable real identities, change groups, inspect private gateway logs, or inject network/storage failures.",
    "Synthetic fixture data, versions, share records and explicit offline grants are intentionally retained; no purge or account deletion.",
]


def new_report(selected):
    return {"schema_version": 1, "suite": "deployed-api", "execution": "not_run", "status": "not_run",
            "started_at": datetime.now(timezone.utc).isoformat(),
            "cases": [{"id": name, "acceptance_gate": CASE_IDS[name], "scenario": name, "status": "not_run", "checks": []}
                      for name in selected], "limitations": LIMITATIONS}


def reserve_report(output: str | None, credentials_path: str | None):
    """Create a new private report before login/writes; keep its owned handle."""
    if not output:
        return None
    target = Path(output)
    try:
        if credentials_path:
            source = Path(credentials_path)
            require(target.resolve() != source.resolve(), "report_cannot_replace_credentials")
            if target.exists() and source.exists():
                require(not os.path.samefile(target, source), "report_cannot_replace_credentials")
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        return os.fdopen(fd, "w", encoding="utf-8")
    except DriverError:
        raise
    except FileExistsError:
        raise DriverError("report_output_must_be_new") from None
    except OSError:
        raise DriverError("report_write_failed") from None


def write_report(report: dict, output: str | None, credentials_path: str | None, reserved=None):
    data = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    handle = reserved if reserved is not None else reserve_report(output, credentials_path)
    if handle is not None:
        try:
            with handle:
                handle.write(data)
                handle.flush()
        except OSError:
            raise DriverError("report_write_failed") from None
    print(data, end="")


class Suite:
    def __init__(self, config: Config):
        self.config = config
        self.owner = API(config)
        self.reader = None
        self.search_http = Transport(config) if config.meili_url else None
        self.prefix = "api-accept-" + secrets.token_hex(8)
        self.case = None

    def check(self, condition, name):
        self.case["checks"].append({"name": name, "status": "pass" if condition else "fail"})
        require(condition, "assertion_failed")

    @staticmethod
    def key():
        return "accept-" + secrets.token_hex(16)

    def stat(self, fid):
        return self.owner.json("GET", "/v1/files/" + safe_id(fid))

    def quota(self):
        q = self.owner.json("GET", "/v1/me").get("quota", {})
        require(all(type(q.get(k)) is int for k in ("used_bytes", "reserved_bytes")), "invalid_quota_response")
        return q["used_bytes"], q["reserved_bytes"]

    def commit(self, response):
        op = safe_id(response.get("operation_id"))
        fid = safe_id(response.get("file_id"))
        deadline = time.monotonic() + 120
        while response.get("state") in ("prepared", "executing", "remote_confirmed", "outcome_unknown"):
            require(time.monotonic() < deadline, "operation_pending_or_unknown_no_blind_retry")
            time.sleep(1)
            response = self.owner.json("GET", "/v1/operations/" + op)
        require(response.get("state") == "committed", "mutation_not_committed")
        return fid

    def mkdir(self, name, parent=""):
        body = {"name": name, "parent_id": parent, "idempotency_key": self.key()}
        if parent:
            body["expected_parent_revision"] = self.stat(parent)["metadata_revision"]
        return self.commit(self.owner.json("POST", "/v1/directories", body))

    def upload(self, name, payload: bytes, parent="", fid=""):
        body = {"name": name, "parent_id": parent, "size_bytes": len(payload),
                "content_type": "text/plain", "idempotency_key": self.key()}
        if fid:
            current = self.stat(fid)
            body.update(file_id=fid, expected_version_id=current["current_version_id"],
                        expected_metadata_revision=current["metadata_revision"])
        u = self.owner.json("POST", "/v1/uploads", body)
        uid = safe_id(u.get("id"))
        file_id = safe_id(u.get("file_id"))
        require(not fid or file_id == fid, "overwrite_changed_file_identity")
        response = self.owner.request("PATCH", "/v1/tus/" + uid, payload, {
            "Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
        require(response.status == 204, "tus_patch_rejected_or_unknown")
        require(response.headers.get("Upload-Offset") == str(len(payload)), "tus_offset_mismatch")
        deadline = time.monotonic() + 120
        while True:
            result = self.owner.json("POST", "/v1/uploads/" + uid + "/finalize", {})
            if result.get("status") in ("saved", "committed"):
                require(result.get("file_id") == file_id, "finalize_changed_file_identity")
                return file_id, safe_id(result.get("version_id"))
            require(result.get("status") in ("uploading", "outcome_unknown") and time.monotonic() < deadline,
                    "upload_not_finalized")
            time.sleep(1)

    def content(self, fid, version, payload, label, api=None):
        path = "/v1/files/" + safe_id(fid) + "/content"
        if version:
            path += "?" + urllib.parse.urlencode({"version_id": safe_id(version)})
        response = (api or self.owner).request("GET", path)
        self.check(response.status == 200 and response.body == payload, label)

    def version_ids(self, fid):
        rows = self.owner.json("GET", "/v1/files/" + safe_id(fid) + "/versions").get("items")
        require(isinstance(rows, list), "invalid_version_response")
        return rows

    def delete(self, fid):
        meta = self.stat(fid)
        actual = self.commit(self.owner.json("DELETE", "/v1/files/" + safe_id(fid), {
            "expected_metadata_revision": meta["metadata_revision"], "idempotency_key": self.key()}))
        require(actual == fid, "delete_changed_file_identity")
        rows = self.owner.json("GET", "/v1/trash").get("items", [])
        found = [row for row in rows if row.get("root_file_id") == fid and row.get("state") == "trashed"]
        require(len(found) == 1, "committed_trash_entry_missing")
        return found[0]

    def restore(self, trash, name=None, parent=None):
        body = {"expected_metadata_revision": trash["file"]["metadata_revision"], "idempotency_key": self.key()}
        if name is not None:
            body["target_name"] = name
        if parent is not None:
            body["target_parent_id"] = parent
        return self.commit(self.owner.json("POST", "/v1/trash/" + safe_id(trash["id"]) + "/restore", body))

    def denied_current(self, fid, label, api=None):
        """Current-entry reads are gated; owner pinned history is intentionally not."""
        for suffix in ("", "/content", "/markdown"):
            r = (api or self.owner).request("GET", "/v1/files/" + safe_id(fid) + suffix)
            self.check(r.status in (403, 404), label + (suffix or "/stat"))
        r = (api or self.owner).request("GET", "/v1/files/" + safe_id(fid) + "/content", headers={"Range": "bytes=0-3"})
        self.check(r.status in (403, 404), label + "/range")

    def denied(self, fid, label, api=None, version=None):
        """Nonowners must also lose retained-history access after share disable."""
        self.denied_current(fid, label, api)
        client = api or self.owner
        response = client.request("GET", "/v1/files/" + safe_id(fid) + "/versions")
        self.check(response.status in (403, 404), label + "/versions")
        if version:
            path = "/v1/files/" + safe_id(fid) + "/content?" + urllib.parse.urlencode({"version_id": safe_id(version)})
            self.check(client.request("GET", path).status in (403, 404), label + "/pinned_history")
            self.check(client.request("GET", path, headers={"Range": "bytes=0-3"}).status in (403, 404),
                       label + "/pinned_history_range")

    def retained_versions(self):
        baseline = self.quota()
        self.check(baseline[1] == 0, "fixture_has_no_pending_quota_reservations")
        name = self.prefix + "-ten.txt"
        fid = ""
        versions = []
        for n in range(1, 11):
            payload = ("synthetic retained version %02d\n" % n).encode() + b"x" * (n * 101)
            fid, vid = self.upload(name, payload, fid=fid)
            versions.append((vid, payload))
            self.check(self.quota() == (baseline[0] + len(payload), 0), "version_%02d_latest_only_quota" % n)
        rows = self.version_ids(fid)
        self.check(len(rows) == 10 and {r.get("id") for r in rows} == {v for v, _ in versions}
                   and {r.get("version_seq") for r in rows} == set(range(1, 11)), "exactly_ten_immutable_versions")
        for n, (vid, payload) in enumerate(versions, 1):
            self.content(fid, vid, payload, "before_delete_version_%02d" % n)
        before_delete = self.quota()
        trash = self.delete(fid)
        self.check(self.quota() == before_delete, "delete_retains_logical_quota")
        self.denied_current(fid, "deleted_current_file_hidden")
        self.check({r.get("id") for r in self.version_ids(fid)} == {v for v, _ in versions}, "deleted_owner_version_history_retained")
        for n, (vid, payload) in enumerate(versions, 1):
            self.content(fid, vid, payload, "while_deleted_owner_version_%02d" % n)
        self.check(self.restore(trash) == fid, "restore_preserves_file_id")
        self.check(self.quota() == before_delete, "restore_does_not_double_charge")
        self.check({r.get("id") for r in self.version_ids(fid)} == {v for v, _ in versions}, "restored_version_ids_unchanged")
        for n, (vid, payload) in enumerate(versions, 1):
            self.content(fid, vid, payload, "after_restore_version_%02d" % n)

    def wait_search(self, marker, expected, label):
        """Bounded real API/index visibility, never a simulated worker completion."""
        deadline = time.monotonic() + 120
        while True:
            result = self.owner.json("POST", "/v1/search", {"query": marker, "limit": 100})
            if search_matches(result, expected):
                self.check(True, label)
                return result
            if time.monotonic() >= deadline:
                self.check(False, label)
            time.sleep(1)

    def wait_raw_search(self, expected, label):
        if self.search_http is None:
            return
        deadline = time.monotonic() + 120
        body = raw_search_request(expected)
        headers = {"Authorization": "Bearer " + self.config.meili_key} if self.config.meili_key else {}
        url = self.config.meili_url + "/indexes/" + self.config.meili_index + "/search"
        while True:
            response = self.search_http.request("POST", url, body, headers)
            require(response.status == 200, "raw_search_http_failure_" + str(response.status))
            if raw_search_matches(response.json(), expected):
                self.check(True, label)
                return
            if time.monotonic() >= deadline:
                self.check(False, label)
            time.sleep(1)

    def search_hidden(self, marker, label):
        # Do not wait for index cleanup: the API must reauthorize immediately.
        result = self.owner.json("POST", "/v1/search", {"query": marker, "limit": 100})
        self.check(result.get("items") == [] and not result.get("next_cursor"), label)

    def subtree(self):
        self.case["raw_index_scope"] = "pending" if self.search_http is not None else "not_checked"
        root = self.mkdir(self.prefix + "-tree")
        nested = self.mkdir("nested", root)
        destination = self.mkdir(self.prefix + "-destination")
        children = []
        search_marker = "subtreesearch" + secrets.token_hex(16)
        child_markers = {}
        for parent, name in ((root, "direct.txt"), (nested, "deep.txt")):
            unique_marker = search_marker + name.split(".")[0]
            old = (search_marker + " " + unique_marker + " subtree original " + name).encode()
            new = (search_marker + " " + unique_marker + " subtree revised " + name).encode()
            fid, v1 = self.upload(name, old, parent)
            _, v2 = self.upload(name, new, parent, fid)
            children.append((fid, parent, name, [(v1, old), (v2, new)]))
            child_markers[fid] = unique_marker
        expected_search = {fid: {"version_id": versions[-1][0], "path": self.stat(fid)["path"],
                                 "marker": child_markers[fid]} for fid, _, _, versions in children}
        published = self.wait_search(search_marker, expected_search, "subtree_current_versions_really_converted_and_indexed")
        for hit in published["items"]:
            expected_search[hit["file_id"]]["artifact_set_id"] = hit["artifact_set_id"]
        self.wait_raw_search(expected_search, "raw_index_initial_versions_artifacts_and_source_paths")
        old_root = self.stat(root)
        moved = self.commit(self.owner.json("PATCH", "/v1/files/" + root + "/location", {
            "target_parent_id": destination, "target_name": "moved-tree", "expected_metadata_revision": old_root["metadata_revision"],
            "idempotency_key": self.key()}))
        self.check(moved == root, "move_preserves_directory_id")
        moved_path = self.stat(destination)["path"] + "/moved-tree"
        self.check(self.stat(root)["path"] == moved_path, "root_path_updated")
        self.check(self.stat(nested)["parent_id"] == root and self.stat(nested)["path"] == moved_path + "/nested",
                   "nested_identity_and_path_updated")
        for fid, parent, name, versions in children:
            meta = self.stat(fid)
            expected = moved_path + ("/nested" if parent == nested else "") + "/" + name
            self.check(meta["id"] == fid and meta["parent_id"] == parent and meta["path"] == expected,
                       "moved_child_identity_path_" + name)
            self.check({r.get("id") for r in self.version_ids(fid)} == {v for v, _ in versions},
                       "move_creates_no_versions_" + name)
            for n, (vid, data) in enumerate(versions):
                self.content(fid, vid, data, "moved_history_" + name + str(n))
        for fid, parent, name, versions in children:
            expected_search[fid]["path"] = moved_path + ("/nested" if parent == nested else "") + "/" + name
        self.wait_search(search_marker, expected_search, "moved_search_preserves_versions_artifacts_and_updates_paths")
        self.wait_raw_search(expected_search, "raw_index_move_source_paths_converged_without_reconversion")
        before_delete = self.quota()
        trash = self.delete(root)
        self.search_hidden(search_marker, "deleted_subtree_immediately_absent_from_search")
        for fid in (root, nested, *(row[0] for row in children)):
            self.denied_current(fid, "deleted_subtree_current_gate")
        self.check(self.owner.request("GET", "/v1/files?parent_id=" + nested).status in (403, 404), "deleted_subdirectory_listing_denied")
        self.check(self.quota() == before_delete, "subtree_delete_retains_quota")
        self.check(self.version_ids(root) == [] and self.version_ids(nested) == [], "deleted_directory_owner_history_authorized")
        for fid, parent, name, versions in children:
            self.check({r.get("id") for r in self.version_ids(fid)} == {v for v, _ in versions},
                       "deleted_child_owner_history_ids_" + name)
            for n, (vid, data) in enumerate(versions):
                self.content(fid, vid, data, "deleted_child_owner_history_" + name + str(n))
        self.search_hidden(search_marker, "deleted_subtree_still_absent_after_owner_history_reads")
        self.check(self.restore(trash, "restored-tree", destination) == root, "subtree_restore_preserves_root")
        restored_path = self.stat(destination)["path"] + "/restored-tree"
        self.check(self.stat(nested)["path"] == restored_path + "/nested", "restored_nested_path")
        for fid, parent, name, versions in children:
            meta = self.stat(fid)
            expected = restored_path + ("/nested" if parent == nested else "") + "/" + name
            self.check(meta["id"] == fid and meta["parent_id"] == parent and meta["path"] == expected,
                       "restored_child_identity_path_" + name)
            self.check({r.get("id") for r in self.version_ids(fid)} == {v for v, _ in versions},
                       "restore_creates_no_versions_" + name)
            for n, (vid, data) in enumerate(versions):
                self.content(fid, vid, data, "restored_history_" + name + str(n))
        self.check(self.quota() == before_delete, "subtree_restore_retains_quota")
        for fid, parent, name, versions in children:
            expected_search[fid]["path"] = restored_path + ("/nested" if parent == nested else "") + "/" + name
        self.wait_search(search_marker, expected_search, "restored_search_preserves_versions_artifacts_and_updates_paths")
        self.wait_raw_search(expected_search, "raw_index_restore_source_paths_converged_without_reconversion")
        if self.search_http is not None:
            self.case["raw_index_scope"] = "verified"

    def approve_share(self, fid, reader_id):
        req = self.owner.json("POST", "/v1/share-requests", {"file_id": fid, "audience_type": "user",
            "audience_id": reader_id, "version_policy": "latest", "rights": ["read", "download"]})
        rid = safe_id(req.get("id"))
        path = "/v1/share-requests/" + rid + "/approve"
        denied = self.owner.request("POST", path, {})
        self.check(denied.status == 403 and denied.json().get("error", {}).get("code") == "CONFIRMATION_REQUIRED",
                   "share_approval_requires_interactive_csrf")
        session = self.owner.json("POST", "/v1/session", {})
        require(isinstance(session.get("csrf_token"), str), "session_csrf_missing")
        return self.owner.json("POST", path, {}, {"X-CSRF-Token": session["csrf_token"]})

    def restore_conflict(self):
        self.reader = API(self.config, "reader")
        recipient = self.reader.login("openid drive.read")
        self.check(recipient.get("id") != self.owner.json("GET", "/v1/me").get("id"), "independent_recipient_principal")
        name = self.prefix + "-collision.txt"
        original = b"retained original share fixture"
        replacement = b"different conflicting file"
        fid, vid = self.upload(name, original)
        self.denied(fid, "before_share_private", self.reader, vid)
        grant = self.approve_share(fid, safe_id(recipient.get("id")))
        self.content(fid, "", original, "approved_share_download", self.reader)
        trash = self.delete(fid)
        self.denied(fid, "deleted_share_denied", self.reader, vid)
        occupant, _ = self.upload(name, replacement)
        before_conflict = self.quota()
        response = self.owner.request("POST", "/v1/trash/" + safe_id(trash["id"]) + "/restore", {
            "expected_metadata_revision": trash["file"]["metadata_revision"], "idempotency_key": self.key()})
        self.check(response.status == 409 and response.json().get("error", {}).get("code") == "PATH_CONFLICT",
                   "same_name_restore_returns_path_conflict")
        self.content(occupant, "", replacement, "collision_does_not_overwrite_occupant")
        self.check(self.quota() == before_conflict, "collision_does_not_change_quota")
        self.check(self.restore(trash, self.prefix + "-restored.txt") == fid, "new_target_restore_preserves_id")
        self.content(fid, vid, original, "restored_history_original_bytes")
        self.denied(fid, "restore_does_not_reactivate_share", self.reader, vid)
        shares = self.owner.json("GET", "/v1/shares").get("items", [])
        old = [s for s in shares if s.get("id") == grant.get("id")]
        self.check(len(old) == 1 and old[0].get("state") == "disabled", "original_share_remains_disabled")
        fresh = self.approve_share(fid, safe_id(recipient.get("id")))
        self.check(fresh.get("id") != grant.get("id"), "fresh_confirmation_creates_new_grant")
        self.content(fid, "", original, "newly_confirmed_share_download", self.reader)
        self.check(self.quota() == before_conflict, "restore_and_sharing_do_not_double_charge")

    def batch100(self):
        root = self.mkdir(self.prefix + "-batch")
        existing = self.mkdir("entry-050", root)
        revision = self.stat(root)["metadata_revision"]
        items = [{"item_id": "item-%03d" % n, "command": "mkdir", "depends_on": [],
                  "idempotency_key": self.key(), "input": {"parent_id": root, "expected_parent_revision": revision,
                  "name": "entry-%03d" % n}} for n in range(100)]
        body = {"idempotency_key": self.key(), "items": items}
        result = self.owner.json("POST", "/v1/batches", body)
        assert_batch_result(result)
        self.check(True, "100_items_99_success_one_path_conflict")
        original_ids = batch_identities(result)
        replay = self.owner.json("POST", "/v1/batches", body)
        assert_batch_result(replay)
        self.check(replay.get("batch_id") == result.get("batch_id") and batch_identities(replay) == original_ids,
                   "same_batch_replay_returns_same_operations_and_files")
        # A new envelope with the same 99 per-item keys must still replay successes.
        independent_retry = self.owner.json("POST", "/v1/batches", {"idempotency_key": self.key(), "items": items})
        assert_batch_result(independent_retry)
        self.check(batch_identities(independent_retry) == original_ids, "new_batch_stable_item_keys_do_not_duplicate_successes")
        page = self.owner.json("GET", "/v1/files?" + urllib.parse.urlencode({"parent_id": root, "limit": 100}))
        rows = page.get("items", [])
        self.check(len(rows) == 100 and not page.get("next_cursor") and len({r.get("id") for r in rows}) == 100,
                   "exactly_100_directories_after_retries")
        self.check({r.get("name") for r in rows} == {"entry-%03d" % n for n in range(100)}, "no_duplicate_names")
        self.check(next((r.get("id") for r in rows if r.get("name") == "entry-050"), None) == existing,
                   "conflicting_original_directory_unchanged")

    def unauthorized(self):
        http = Transport(self.config)  # Separate cookie jar, no fabricated or cached tokens.
        sentinel = "00000000-0000-4000-8000-000000000001"
        for method, template in AUTH_ROUTES:
            path = template.replace(":id", sentinel).replace(":artifact", sentinel).replace(":asset", "synthetic.png").replace(":page", "1")
            headers = {"Origin": self.config.public}
            body = {} if method in ("POST", "DELETE", "PATCH") else None
            if method == "PATCH" and "/tus/" in path:
                body = b"x"
                headers.update({"Content-Type": "application/offset+octet-stream", "Tus-Resumable": "1.0.0", "Upload-Offset": "0"})
            response = http.request(method, self.config.public + path, body, headers)
            self.check(response.status == 401, method + " " + template + " requires_bearer")
        response = http.request("GET", self.config.public + "/v1/files/" + sentinel + "/content",
                                headers={"Origin": self.config.public, "Range": "bytes=0-3"})
        self.check(response.status == 401, "anonymous_range_cannot_bypass_bearer")


def raw_search_request(expected):
    require(bool(expected) and len(expected) <= 10, "invalid_raw_search_fixture_scope")
    clauses = []
    for fid, wanted in expected.items():
        file_id = safe_id(fid)
        artifact = safe_id(wanted.get("artifact_set_id"))
        clauses.append('(file_id = "' + file_id + '" AND artifact_set_id = "' + artifact + '")')
    return {"q": "", "filter": " OR ".join(clauses), "limit": 100,
            "attributesToRetrieve": ["file_id", "version_id", "artifact_set_id", "source_path"]}


def raw_search_matches(result, expected):
    require(isinstance(result, dict) and isinstance(result.get("hits"), list), "invalid_raw_search_response")
    rows = result["hits"]
    require(all(isinstance(row, dict) for row in rows), "invalid_raw_search_hit")
    # Tiny text fixtures produce one block each; reject possible truncation.
    if len(rows) >= 100 or {row.get("file_id") for row in rows} != set(expected):
        return False
    return all(row.get("version_id") == expected[row["file_id"]]["version_id"]
               and row.get("artifact_set_id") == expected[row["file_id"]]["artifact_set_id"]
               and row.get("source_path") == expected[row["file_id"]]["path"] for row in rows)


def search_matches(result, expected):
    """Match actual REST fields, including every hit, not a single lucky result.

    path is the application's reauthorized DB projection. This checks public
    search semantics, not a private Meilisearch document's raw source_path.
    """
    require(bool(expected), "empty_search_fixture_scope")
    require(isinstance(result, dict) and isinstance(result.get("items"), list), "invalid_search_response")
    rows = result["items"]
    require(all(isinstance(row, dict) for row in rows), "invalid_search_hit")
    if result.get("next_cursor") or {row.get("file_id") for row in rows} != set(expected):
        return False
    artifacts = {}
    for row in rows:
        wanted = expected[row["file_id"]]
        if (row.get("returned_version_id") != wanted["version_id"]
                or row.get("current_version_id") != wanted["version_id"]
                or row.get("path") != wanted["path"] or row.get("stale") is not False
                or row.get("newer_version_status") != "ready"
                or not isinstance(row.get("completeness"), dict) or row["completeness"].get("status") != "ready"
                or not isinstance(row.get("snippet"), str) or wanted["marker"] not in row["snippet"]):
            return False
        artifact = safe_id(row.get("artifact_set_id"))
        if wanted.get("artifact_set_id", artifact) != artifact:
            return False
        artifacts.setdefault(row["file_id"], set()).add(artifact)
    return all(len(values) == 1 for values in artifacts.values())


def assert_batch_result(result):
    rows = result.get("items", [])
    require(result.get("state") == "completed" and len(rows) == 100 and len(result.get("succeeded", [])) == 99
            and len(result.get("failed", [])) == 1 and result.get("pending") == [], "batch_not_99_success_one_failure")
    require({r.get("item_id") for r in rows} == {"item-%03d" % n for n in range(100)}, "batch_missing_or_duplicate_items")
    succeeded = [r for r in rows if r.get("status") == "succeeded"]
    failed = [r for r in rows if r.get("status") == "failed"]
    require(result["succeeded"] == succeeded and result["failed"] == failed, "batch_summary_disagrees_with_items")
    for r in rows:
        if r.get("item_id") == "item-050":
            require(r.get("status") == "failed" and r.get("error", {}).get("code") == "PATH_CONFLICT", "batch_wrong_conflict")
        else:
            require(r.get("status") == "succeeded", "batch_independent_item_failed")


def batch_identities(result):
    identities = {}
    for row in result["succeeded"]:
        value = row.get("result", {}).get("value", {})
        require(value.get("state") == "committed", "batch_uncommitted_success")
        identities[row["item_id"]] = (safe_id(value.get("operation_id")), safe_id(value.get("file_id")))
    require(len(identities) == 99, "batch_duplicate_success_identity")
    require(len({v[1] for v in identities.values()}) == 99, "batch_duplicate_created_file")
    return identities


def parse_scenarios(value):
    if value == "all":
        return list(CASE_IDS)
    parts = value.split(",")
    require(parts and len(parts) == len(set(parts)) and all(p in CASE_IDS for p in parts), "invalid_scenario_selection")
    return parts


def main(argv=None, env=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", default="all", help="all or comma-separated: " + ",".join(CASE_IDS))
    parser.add_argument("--check-only", action="store_true", help="local configuration validation only, never acceptance PASS")
    parser.add_argument("--output", help="sanitized JSON evidence output; no tokens or upstream bodies")
    parser.add_argument("--allow-disposable-writes", action="store_true", help="explicitly permit synthetic data writes and fixture share approvals; offline consent requires its separate environment opt-in")
    parser.add_argument("--allow-loopback-http", action="store_true", help="allow cleartext only for literal loopback IPs or exact localhost")
    args = parser.parse_args(argv)
    env = dict(os.environ if env is None else env)
    try:
        selected = parse_scenarios(args.scenario)
    except DriverError:
        parser.error("invalid scenario selection")
    report = new_report(selected)
    rc = 2
    report_handle = None
    output_ready = args.output is None
    try:
        report_handle = reserve_report(args.output, env.get("TEST_CREDENTIALS_FILE"))
        output_ready = True
        config = Config.from_environment(env, args.allow_loopback_http, "restore_conflict" in selected)
        report["configuration"] = "valid"
        if args.check_only:
            report["reason"] = "configuration_only_no_network_or_acceptance_execution"
            rc = 3
        else:
            require(args.allow_disposable_writes, "explicit_disposable_write_opt_in_required")
            needs_identity = any(name != "unauthorized" for name in selected)
            require(not needs_identity or env.get("ALLOW_DISPOSABLE_TEST_OIDC_CONSENT") == "yes",
                    "explicit_disposable_offline_consent_required")
            suite = Suite(config)
            report["fixture_prefix"] = suite.prefix
            report["execution"] = "executed"
            # Anonymous-only route checks need no IdP session or offline grant.
            if needs_identity:
                suite.owner.login()
                suite.owner.delegate(allow_new_grant=True)
            for case in report["cases"]:
                suite.case = case
                try:
                    getattr(suite, case["scenario"])()
                    case["status"] = "pass"
                except DriverError as e:
                    case["status"] = "fail"
                    case["reason"] = str(e)
                    raise
            report["status"] = "pass"
            rc = 0
    except DriverError as e:
        report["reason"] = str(e)
        if report["execution"] == "executed":
            report["status"] = "fail"
            rc = 1
    except KeyboardInterrupt:
        report["reason"] = "interrupted_outcome_may_be_unconfirmed"
        report["status"] = "fail" if report["execution"] == "executed" else "not_run"
        rc = 130
    except Exception:
        # Do not expose upstream bodies, token-bearing URLs, credentials or traceback locals.
        report["reason"] = "unexpected_driver_error_details_suppressed"
        report["status"] = "fail" if report["execution"] == "executed" else "not_run"
        rc = 1 if report["execution"] == "executed" else 2
    report["finished_at"] = datetime.now(timezone.utc).isoformat()
    try:
        write_report(report, args.output if output_ready else None, env.get("TEST_CREDENTIALS_FILE"), report_handle)
    except DriverError as e:
        print(json.dumps({"schema_version": 1, "suite": "deployed-api", "status": "fail", "reason": str(e)}))
        return 2
    return rc


if __name__ == "__main__":
    sys.exit(main())
