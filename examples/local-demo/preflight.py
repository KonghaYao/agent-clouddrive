#!/usr/bin/env python3
"""Genuine own-user native checks for the isolated one-command example.

This is NOT a native final-commit CAS test or application acceptance. oCIS tus
preconditions only guard admission; durable application operation ownership is
still mandatory. Current GET If-Match enforcement is observed, never assumed.
Only generated synthetic files are created and retained. No DELETE, cleanup,
mutation retry or administrator impersonation is implemented.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import fcntl
import os
import stat
import json
from pathlib import Path
import re
import secrets
import time
import urllib.parse
import xml.etree.ElementTree as ET

from demo_support import (DemoTransport, DriverError, audiences, jwt_claims,
                          load_runtime, origin, private_json, private_write, require)

PROOFS = (
    "pkce", "confidential_exchange_openid", "personal_space", "tus_create",
    "tus_overwrite", "revision_read", "move_stable_id", "move_retained_history",
    "stale_admission_denied",
)
LIMITATIONS = {
    "native_final_commit_cas": False,
    "current_get_if_match_enforcement": False,
    "business_durable_operation_gates_required": True,
}
PROPERTIES = (b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:" '
              b'xmlns:oc="http://owncloud.org/ns"><d:prop><oc:fileid/>'
              b'<d:getetag/><d:getcontentlength/><d:resourcetype/></d:prop></d:propfind>')
FIRST = b"CloudDrive native demo preflight: retained first version.\n"
SECOND = b"CloudDrive native demo preflight: retained second version.\n"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def segment(value):
    require(isinstance(value, str) and 0 < len(value) <= 1024
            and value not in (".", "..") and not any(c in value for c in "/\\%\x00\r\n")
            and not any(c.isspace() or ord(c) < 32 for c in value), "native_identifier_invalid")
    return value


def quoted(value):
    return urllib.parse.quote(segment(value), safe="")


def etag(value):
    require(isinstance(value, str) and 0 < len(value) <= 1024
            and not value.startswith("W/") and not any(ord(c) < 32 for c in value),
            "native_etag_missing_or_weak")
    return value.strip('"')


def safe_native_url(value, native, *, prefix, query=False):
    """Never follow a native href/Location to a foreign origin or escaped path."""
    require(isinstance(value, str) and bool(value), "native_endpoint_missing")
    url = urllib.parse.urljoin(native + "/", value)
    try:
        p = urllib.parse.urlsplit(url)
        decoded = urllib.parse.unquote(p.path, errors="strict")
        valid = (origin(url) == native and not p.username and not p.password
                 and not p.fragment and (query or not p.query)
                 and not any(c.isspace() or ord(c) < 32 for c in url)
                 and "\\" not in url and "\\" not in decoded and "%" not in decoded
                 and not any(c.isspace() or ord(c) < 32 for c in decoded)
                 and not any(s in (".", "..") for s in decoded.split("/"))
                 and decoded.startswith(prefix))
    except (ValueError, UnicodeError):
        valid = False
    require(valid, "untrusted_native_endpoint")
    return url


def xml_rows(body):
    require(b"<!DOCTYPE" not in body.upper() and b"<!ENTITY" not in body.upper(),
            "native_xml_entities_forbidden")
    try:
        root = ET.fromstring(body)
    except ET.ParseError:
        raise DriverError("invalid_native_multistatus") from None
    require(root.tag == "{DAV:}multistatus", "invalid_native_multistatus")
    rows = []
    for response in root.findall("{DAV:}response"):
        row = {"href": response.findtext("{DAV:}href", "")}
        for propstat in response.findall("{DAV:}propstat"):
            if " 200 " not in propstat.findtext("{DAV:}status", ""):
                continue
            prop = propstat.find("{DAV:}prop")
            if prop is None:
                continue
            for key, name in (("resource_id", "{http://owncloud.org/ns}fileid"),
                              ("etag", "{DAV:}getetag"), ("size", "{DAV:}getcontentlength")):
                found = prop.findtext(name)
                if found is not None:
                    row[key] = found
            row["directory"] = prop.find("{DAV:}resourcetype/{DAV:}collection") is not None
        rows.append(row)
    return rows


class NativeProbe:
    def __init__(self, runtime):
        self.runtime = runtime
        self.root = runtime.root
        self.project = runtime.state["project"]
        # The ordinary demo fixes its native origin. A programmatic native-test
        # runtime may supply a strict, CA-verified loopback transport instead.
        self.native = getattr(runtime, "native_origin", "http://ocis:9200")
        self.http = (runtime.native_http() if hasattr(runtime, "native_http")
                     else DemoTransport(runtime, native=True))
        self.access = ""
        self.journal_path = self.root / "preflight-journal.json"
        self.result_path = self.root / "preflight-result.json"
        self.journal = None
        self.proofs = {name: False for name in PROOFS}

    def save(self):
        self.journal["proofs"] = self.proofs.copy()
        private_write(self.journal_path, self.journal)

    def mark(self, name):
        self.proofs[name] = True
        if self.journal is not None:
            self.save()

    def read(self, method, url, body=None, headers=None):
        # All read URLs are validated before transmitting the delegated token.
        url = safe_native_url(url, self.native, prefix="/")
        return self.http.request(method, url, body, {"Authorization": "Bearer " + self.access, **(headers or {})})

    def mutate(self, label, method, url, body, headers, *, target):
        require(method in ("POST", "PATCH", "MKCOL", "MOVE"), "mutation_not_permitted")
        url = safe_native_url(url, self.native, prefix="/data/" if method == "PATCH" else "/dav/spaces/",
                              query=method == "PATCH")
        record = {"id": secrets.token_hex(16), "label": label, "method": method,
                  "target": target, "status": "dispatch_pending", "body_sha256": digest(body),
                  "body_base64": base64.b64encode(body).decode(),
                  "headers": {k: v for k, v in headers.items() if k != "Destination"}}
        if method == "PATCH":
            # Signed upload URLs are credentials: persist a fingerprint, not the
            # URL. The durable POST intent + synthetic path permits inspection.
            record["upload_location_sha256"] = digest(url.encode())
        elif method == "MOVE":
            destination = safe_native_url(headers.get("Destination"), self.native, prefix="/dav/spaces/")
            record["destination_path"] = urllib.parse.urlsplit(destination).path
        self.journal["operations"].append(record)
        self.save()  # Intent, generated bytes and known IDs are durable BEFORE I/O.
        try:
            result = self.http.request(method, url, body, {"Authorization": "Bearer " + self.access, **headers})
        except Exception:
            record["status"] = "outcome_unknown"
            self.journal["status"] = "outcome_unknown"
            self.journal["blocked_operation"] = label
            self.save()
            raise DriverError("native_mutation_outcome_unknown_" + label + "_inspect_retained_journal") from None
        record["http_status"] = result.status
        if result.status >= 500:
            record["status"] = "outcome_unknown"
            self.journal["status"] = "outcome_unknown"
            self.journal["blocked_operation"] = label
            self.save()
            raise DriverError("native_mutation_outcome_unknown_" + label + "_inspect_retained_journal")
        record["status"] = "response_received"
        for header, key in (("Oc-Fileid", "resource_id"), ("ETag", "etag"), ("Upload-Offset", "offset")):
            value = result.headers.get(header)
            if value:
                record[key] = value
        location = result.headers.get("Location")
        if location:
            record["upload_location_sha256"] = digest(location.encode())
        self.save()
        return result

    def authenticate(self):
        api = self.runtime.api("owner")
        profile = api.oauth_login()  # Real generated user's authorization-code PKCE, no app call.
        subject = jwt_claims(api.access)
        require(isinstance(profile.get("sub"), str) and profile["sub"] == subject.get("sub"),
                "native_subject_missing")
        self.mark("pkce")
        credentials = private_json(self.root / "service-credentials.json")
        require(all(isinstance(credentials.get(k), str) and bool(credentials[k]) for k in
                    ("exchange_client_id", "exchange_client_secret")), "exchange_service_credentials_missing")
        response = api.http.form(api.token_endpoint, {
            "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
            "client_id": credentials["exchange_client_id"],
            "client_secret": credentials["exchange_client_secret"],
            "subject_token": api.access,
            "subject_token_type": "urn:ietf:params:oauth:token-type:access_token",
            "requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
            "audience": "ocis", "scope": "openid",
        })
        require(response.status == 200, "confidential_native_exchange_failed")
        token = response.json()
        require(token.get("token_type", "").lower() == "bearer", "native_exchange_token_type_invalid")
        self.access = token.get("access_token", "")
        claims = jwt_claims(self.access)
        require(claims.get("sub") == subject["sub"] and claims.get("iss") == self.runtime.issuer
                and "ocis" in audiences(claims) and "openid" in claims.get("scope", "").split(),
                "native_delegated_identity_mismatch")
        # Decoding claims alone is not proof: actual oCIS Graph acceptance below
        # must verify this token before any exchange proof can pass.
        response = self.read("GET", self.native + "/graph/v1.0/me/drives")
        require(response.status == 200, "native_graph_rejected_delegated_user")
        drives = response.json().get("value")
        require(isinstance(drives, list), "native_graph_drives_invalid")
        spaces = [d for d in drives if isinstance(d, dict) and d.get("driveType") == "personal"]
        require(len(spaces) == 1, "native_personal_space_not_unique")
        space = spaces[0]
        root = space.get("root")
        require(isinstance(root, dict), "native_personal_space_root_missing")
        self.dav = safe_native_url(root.get("webDavUrl"), self.native, prefix="/dav/spaces/")
        self.space_id = segment(space.get("id"))
        self.subject = subject["sub"]
        self.mark("confidential_exchange_openid")
        self.mark("personal_space")

    def stat(self, url):
        response = self.read("PROPFIND", url, PROPERTIES, {"Depth": "0", "Content-Type": "application/xml"})
        require(response.status == 207, "native_stat_failed")
        rows = xml_rows(response.body)
        require(len(rows) == 1, "native_stat_not_unique")
        row = rows[0]
        segment(row.get("resource_id"))
        etag(row.get("etag"))
        return row

    def get(self, url, expected, expected_etag):
        response = self.read("GET", url)
        require(response.status == 200 and etag(response.headers.get("ETag")) == etag(expected_etag),
                "native_fixed_read_etag_mismatch")
        require(response.body == expected, "native_fixed_read_bytes_mismatch")

    def revisions(self, resource_id):
        path = "/dav/meta/" + quoted(resource_id) + "/v"
        response = self.read("PROPFIND", self.native + path, PROPERTIES,
                             {"Depth": "1", "Content-Type": "application/xml"})
        require(response.status == 207, "native_history_listing_failed")
        result = []
        expected_prefix = "/dav/meta/" + resource_id + "/v/"
        parsed_rows = xml_rows(response.body)
        if self.journal is not None:
            # Private targeted diagnostics retain native paths/IDs, never URL
            # authorities, query strings, bearer tokens or upstream bodies.
            self.journal["last_history_response"] = {"resource_id": resource_id, "rows": [
                {"path": urllib.parse.urlsplit(row.get("href", "")).path,
                 "same_origin": origin(urllib.parse.urljoin(self.native + "/", row.get("href", ""))) == self.native,
                 "directory": row.get("directory"), "etag_present": bool(row.get("etag"))}
                for row in parsed_rows]}
            self.save()
        for row in parsed_rows:
            href = row.get("href", "")
            url = safe_native_url(href, self.native, prefix="/dav/meta/")
            decoded = urllib.parse.unquote(urllib.parse.urlsplit(url).path)
            # oCIS 8.2.2 returns /dav/meta/{resource}/ as the collection
            # self row even though PROPFIND targets its /v endpoint. Accept
            # only that exact same-resource collection (or exact /v self).
            self_paths = {expected_prefix.rstrip("/"), "/dav/meta/" + resource_id}
            if decoded.rstrip("/") in self_paths:
                require(row.get("directory") is True, "native_history_self_not_collection")
                continue
            require(decoded.startswith(expected_prefix) and "/" not in decoded[len(expected_prefix):].rstrip("/"),
                    "native_revision_href_invalid")
            etag(row.get("etag"))
            result.append({"href": urllib.parse.urlsplit(url).path, "etag": row["etag"]})
        return result

    def old_revision(self, resource_id, old_etag):
        matches = [r for r in self.revisions(resource_id) if etag(r["etag"]) == etag(old_etag)]
        require(len(matches) == 1, "native_exact_old_revision_missing_or_ambiguous")
        revision = matches[0]
        self.get(self.native + revision["href"], FIRST, old_etag)
        return revision

    def uncertain(self, label):
        self.journal["status"] = "outcome_unknown"
        self.journal["blocked_operation"] = label
        self.save()
        raise DriverError("native_mutation_outcome_unknown_" + label + "_inspect_retained_journal")

    def upload(self, directory, name, content, expected_etag=None, label="create"):
        headers = {"Tus-Resumable": "1.0.0", "Upload-Length": str(len(content)),
                   "Upload-Metadata": "filename " + base64.b64encode(name.encode()).decode()}
        headers["If-Match" if expected_etag else "If-None-Match"] = expected_etag or "*"
        response = self.mutate(label + "_admission", "POST", directory, b"", headers,
                               target={"directory_path": urllib.parse.urlsplit(directory).path, "name": name,
                                       "expected_resource_id": self.journal.get("resource_id"),
                                       "content_sha256": digest(content)})
        require(response.status == 201, "native_" + label + "_admission_failed")
        try:
            location = safe_native_url(response.headers.get("Location"), self.native, prefix="/data/", query=True)
        except DriverError:
            self.journal["status"] = "outcome_unknown"
            self.journal["blocked_operation"] = label + "_admission"
            self.save()
            raise DriverError("native_upload_admitted_without_trusted_location_inspect_retained_journal") from None
        response = self.mutate(label + "_content", "PATCH", location, content,
            {"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"},
            target={"directory_path": urllib.parse.urlsplit(directory).path, "name": name,
                    "expected_resource_id": self.journal.get("resource_id")})
        if response.status != 204 or response.headers.get("Upload-Offset") != str(len(content)):
            self.uncertain(label + "_completion_unconfirmed")
        try:
            resource_id = segment(response.headers.get("Oc-Fileid"))
        except DriverError:
            self.uncertain(label + "_native_identity_unconfirmed")
        self.journal["resource_id"] = resource_id
        self.save()  # Persist native ID before subsequent requests or possible read failure.
        stable = self.native + "/dav/spaces/" + quoted(resource_id)
        # Native postprocessing is asynchronous. Only reads are retried.
        metadata = None
        for attempt in range(20):
            try:
                metadata = self.stat(stable)
                self.get(stable, content, metadata["etag"])
                break
            except DriverError:
                if attempt == 19:
                    raise
                time.sleep(0.5)
        require(metadata["resource_id"] == resource_id and not metadata.get("directory"),
                "native_uploaded_resource_identity_mismatch")
        return metadata

    def fresh(self):
        probe_id = secrets.token_hex(16)
        self.journal = {"schema": 1, "kind": "agent-clouddrive-native-preflight-journal",
            "project": self.project, "run_id": probe_id, "status": "running",
            "subject": self.subject, "space_id": self.space_id,
            "directory_name": "clouddrive-native-probe-" + probe_id,
            "source_name": "original.txt", "moved_name": "retained.txt",
            "payloads": {"first": {"sha256": digest(FIRST), "bytes": len(FIRST)},
                         "second": {"sha256": digest(SECOND), "bytes": len(SECOND)}}, "operations": []}
        self.save()
        directory = self.dav.rstrip("/") + "/" + self.journal["directory_name"]
        self.journal["directory_path"] = urllib.parse.urlsplit(directory).path
        self.save()
        response = self.mutate("create_probe_directory", "MKCOL", directory, b"", {"If-None-Match": "*"},
                               target={"directory_path": self.journal["directory_path"]})
        require(response.status == 201, "native_fresh_probe_directory_not_created")
        directory_meta = self.stat(directory)
        require(directory_meta.get("directory") is True, "native_probe_directory_not_collection")
        self.journal["directory_resource_id"] = directory_meta["resource_id"]
        self.save()
        first = self.upload(directory, self.journal["source_name"], FIRST)
        self.journal["first_etag"] = first["etag"]
        self.save()
        self.mark("tus_create")
        # Preserve a distinct native history generation even on timestamp-based stores.
        time.sleep(1.1)
        second = self.upload(directory, self.journal["source_name"], SECOND, first["etag"], "overwrite")
        require(second["resource_id"] == first["resource_id"] and etag(second["etag"]) != etag(first["etag"]),
                "native_overwrite_identity_or_etag_mismatch")
        self.journal["second_etag"] = second["etag"]
        self.save()
        self.mark("tus_overwrite")
        old = self.old_revision(first["resource_id"], first["etag"])
        self.journal["old_revision"] = old
        self.save()
        self.mark("revision_read")
        # This is ONLY a stale admission test, not two sessions racing at commit.
        response = self.mutate("stale_admission", "POST", directory, b"", {
            "Tus-Resumable": "1.0.0", "Upload-Length": "1", "If-Match": first["etag"],
            "Upload-Metadata": "filename " + base64.b64encode(self.journal["source_name"].encode()).decode()},
            target={"directory_path": self.journal["directory_path"], "name": self.journal["source_name"],
                    "expected_resource_id": first["resource_id"]})
        require(response.status in (409, 412), "native_stale_admission_not_denied_no_patch_sent")
        self.mark("stale_admission_denied")
        stable = self.native + "/dav/spaces/" + quoted(first["resource_id"])
        self.get(stable, SECOND, second["etag"])
        # oCIS 8.2.2 ignores stale If-Match on current GET. Either observed result
        # is safe here; application fixed reads must independently verify ETags.
        current = self.read("GET", stable, headers={"If-Match": first["etag"]})
        require(current.status == 412 or (current.status == 200 and current.body == SECOND
                and etag(current.headers.get("ETag")) == etag(second["etag"])),
                "native_current_get_observation_unexpected")
        self.journal["current_get_stale_if_match_observation"] = (
            "precondition_denied" if current.status == 412 else "ignored_returned_current")
        destination = directory + "/" + self.journal["moved_name"]
        response = self.mutate("move_retained_probe", "MOVE", stable, b"", {
            "Destination": destination, "Overwrite": "F", "If-Match": second["etag"]},
            target={"resource_id": first["resource_id"], "source_path": urllib.parse.urlsplit(stable).path})
        require(response.status in (201, 204), "native_move_failed")
        self.verify_retained()
        self.journal["status"] = "passed"
        self.save()

    def verify_retained(self):
        j = self.journal
        require(j.get("subject") == self.subject and j.get("space_id") == self.space_id,
                "native_retained_probe_identity_changed")
        directory = self.native + j["directory_path"]
        directory_meta = self.stat(directory)
        require(directory_meta["resource_id"] == j["directory_resource_id"] and directory_meta.get("directory"),
                "native_retained_probe_directory_changed")
        destination = directory + "/" + segment(j["moved_name"])
        stable = self.native + "/dav/spaces/" + quoted(j["resource_id"])
        moved, stable_meta = self.stat(destination), self.stat(stable)
        require(moved["resource_id"] == j["resource_id"] == stable_meta["resource_id"]
                and etag(moved["etag"]) == etag(j["second_etag"]), "native_move_changed_identity")
        self.get(stable, SECOND, j["second_etag"])
        self.get(destination, SECOND, j["second_etag"])
        require(self.read("GET", directory + "/" + segment(j["source_name"])).status == 404,
                "native_move_source_still_exists")
        self.mark("move_stable_id")
        old = self.old_revision(j["resource_id"], j["first_etag"])
        require(old == j["old_revision"], "native_move_changed_old_revision")
        self.mark("move_retained_history")

    def execute(self):
        old = private_json(self.journal_path) if self.journal_path.exists() or self.journal_path.is_symlink() else None
        if old is not None:
            require(old.get("schema") == 1 and old.get("kind") == "agent-clouddrive-native-preflight-journal"
                    and old.get("project") == self.project, "native_preflight_journal_wrong_runtime")
            require(old.get("status") == "passed" and all(old.get("proofs", {}).get(k) is True for k in PROOFS),
                    "native_previous_probe_incomplete_no_mutation_replay_inspect_retained_journal")
        self.authenticate()
        if old is None:
            self.fresh()
        else:
            self.journal = old
            self.proofs = old["proofs"].copy()
            self.verify_retained()  # A successful rerun is read-only and verifies retained proof.
        require(all(self.proofs.get(k) is True for k in PROOFS), "native_preflight_proofs_incomplete")
        result = {"schema": 1, "kind": "agent-clouddrive-local-demo-native-preflight",
                  "project": self.project, "status": "passed", "proofs": self.proofs.copy(),
                  "limitations": LIMITATIONS.copy(), "run_id": self.journal["run_id"],
                  "checked_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
        private_write(self.result_path, result)
        return result


def run_preflight(runtime):
    """Serialize local probe runs before inspecting or creating their journal."""
    lock = None
    try:
        lock = os.open(runtime.root / "preflight.lock", os.O_RDWR | os.O_CREAT
                       | getattr(os, "O_NOFOLLOW", 0), 0o600)
        info = os.fstat(lock)
        require(stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
                and not info.st_mode & 0o077 and info.st_nlink == 1, "native_preflight_lock_not_private")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise DriverError("native_preflight_already_running") from None
        return _run_locked(runtime)
    except OSError:
        raise DriverError("native_preflight_lock_unavailable") from None
    finally:
        if lock is not None:
            os.close(lock)


def _run_locked(runtime):
    probe = NativeProbe(runtime)
    # Invalidate an old success marker BEFORE checking the live stack again.
    private_write(probe.result_path, {"schema": 1, "kind": "agent-clouddrive-local-demo-native-preflight",
        "project": probe.project, "status": "running", "proofs": probe.proofs.copy(),
        "limitations": LIMITATIONS.copy()})
    try:
        return probe.execute()
    except Exception as error:
        code = str(error) if isinstance(error, DriverError) and re.fullmatch(r"[a-z0-9_]{1,160}", str(error)) else "native_preflight_failed"
        if probe.journal is not None and probe.journal.get("status") != "passed":
            if probe.journal.get("status") != "outcome_unknown":
                probe.journal["status"] = "failed"
            probe.journal["error_code"] = code
            probe.save()
        private_write(probe.result_path, {"schema": 1, "kind": "agent-clouddrive-local-demo-native-preflight",
            "project": probe.project, "status": "failed", "proofs": probe.proofs.copy(),
            "limitations": LIMITATIONS.copy(), "error_code": code})
        raise DriverError(code) from None


def run(runtime, container_network=False):
    return run_preflight(runtime if hasattr(runtime, "root") else load_runtime(runtime, container_network))


def main(argv=None):
    parser = argparse.ArgumentParser(description="Verify the generated disposable demo's real native endpoints")
    parser.add_argument("--runtime", "--runtime-dir", required=True, type=Path)
    parser.add_argument("--container-network", action="store_true")
    args = parser.parse_args(argv)
    try:
        run_preflight(load_runtime(args.runtime, args.container_network))
    except DriverError as error:
        print("Native preflight failed: " + str(error))
        return 1
    print("Native preflight passed. Application durable operation gates remain required.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
