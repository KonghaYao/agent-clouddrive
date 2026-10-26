"""Offline synthetic protocol/negative tests. NEVER native acceptance evidence."""
from __future__ import annotations

import base64
import importlib.util
import json
import fcntl
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
import urllib.parse
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "examples/local-demo"))
import preflight as probe
from demo_support import DriverError, private_json, private_write
from deployed_api_harness import Response

NATIVE = "http://ocis:9200"
ISSUER = "http://drive.localhost:8080/identity/realms/clouddrive"
RID = "storage$space!file"
DIRID = "storage$space!directory"


def token(claims):
    return "synthetic." + base64.urlsafe_b64encode(json.dumps(claims).encode()).rstrip(b"=").decode() + ".not-a-signature"


def response(status=200, body=b"", headers=None):
    if isinstance(body, dict):
        body = json.dumps(body).encode()
    return Response(status, headers or {}, body)


def rows(entries):
    parts = []
    for href, rid, etag, directory in entries:
        parts.append('<d:response><d:href>' + href + '</d:href><d:propstat><d:prop>'
            + ('<oc:fileid>' + rid + '</oc:fileid>' if rid else '')
            + '<d:getetag>' + etag + '</d:getetag><d:getcontentlength>12</d:getcontentlength>'
            + ('<d:resourcetype><d:collection/></d:resourcetype>' if directory else '<d:resourcetype/>')
            + '</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>')
    return ('<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">' + ''.join(parts) + '</d:multistatus>').encode()


class FakeHTTP:
    """In-memory test fixture, not a replacement for the deployed native gate."""
    def __init__(self, root):
        self.root = root
        self.calls = []
        self.forms = []
        self.version = 0
        self.moved = False
        self.directory = None
        self.admissions = 0
        self.lose_patch_response = False
        self.fail_patch_500 = False
        self.foreign_upload = False
        self.admit_stale = False
        self.swap_move_identity = False
        self.drop_history_after_move = False
        self.current_ifmatch_412 = False
        self.history_foreign = False
        self.history_wrong_collection = False
        self.graph_status = 200
        self.journal_before_io = []

    def form(self, url, data):
        self.forms.append((url, data))
        return response(body={"access_token": token({"iss": ISSUER, "sub": "synthetic-owner",
            "aud": "ocis", "scope": "openid"}), "token_type": "Bearer"})

    def request(self, method, url, body=None, headers=None):
        headers = headers or {}
        self.calls.append((method, url, body, headers))
        path = urllib.parse.unquote(urllib.parse.urlsplit(url).path)
        if method in ("POST", "PATCH", "MKCOL", "MOVE"):
            journal = private_json(self.root / "preflight-journal.json")
            self.journal_before_io.append(journal)
            assert journal["operations"][-1]["status"] == "dispatch_pending"
        if path == "/graph/v1.0/me/drives":
            return response(self.graph_status, {"value": [{"id": "storage$space", "driveType": "personal",
                "root": {"webDavUrl": NATIVE + "/dav/spaces/storage$space"}}]})
        if method == "MKCOL":
            self.directory = path
            return response(201)
        if method == "POST":
            if headers.get("If-Match") == '"one"' and self.version == 2 and not self.admit_stale:
                return response(412)
            self.admissions += 1
            location = ("https://attacker.example" if self.foreign_upload else NATIVE) + "/data/session?secret=not-for-journal"
            return response(201, headers={"Location": location})
        if method == "PATCH":
            self.version += 1
            if self.lose_patch_response:
                raise DriverError("network_or_tls_failure_outcome_unconfirmed")
            if self.fail_patch_500:
                return response(502)
            return response(204, headers={"Upload-Offset": str(len(body)), "Oc-Fileid": RID})
        if method == "MOVE":
            self.moved = True
            return response(201)
        if method == "PROPFIND":
            if path.endswith("/v"):
                # Real oCIS 8.2.2 uses /dav/meta/{rid}/ as the self collection.
                parent = path.removesuffix("/v") + "/"
                if self.history_wrong_collection:
                    parent = "/dav/meta/unrelated-resource/"
                entries = [(parent, None, "", True)]
                if self.version == 2 and not (self.moved and self.drop_history_after_move):
                    revision = ("https://attacker.example" if self.history_foreign else "") + path + "/12345"
                    entries.append((revision, None, '"one"', False))
                return response(207, rows(entries))
            if path == self.directory:
                return response(207, rows([(path, DIRID, '"directory"', True)]))
            rid = "storage$space!wrong" if self.moved and self.swap_move_identity and path.endswith("retained.txt") else RID
            return response(207, rows([(path, rid, '"one"' if self.version == 1 else '"two"', False)]))
        if method == "GET":
            if path.endswith("/original.txt") and self.moved:
                return response(404)
            if "/v/" in path:
                return response(body=probe.FIRST, headers={"ETag": '"one"'})
            if headers.get("If-Match") and self.current_ifmatch_412:
                return response(412)
            return response(body=probe.FIRST if self.version == 1 else probe.SECOND,
                headers={"ETag": '"one"' if self.version == 1 else '"two"'})
        raise AssertionError("unexpected synthetic request")


class PreflightTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.http = FakeHTTP(self.root)
        api = types.SimpleNamespace(access=token({"iss": ISSUER, "sub": "synthetic-owner"}),
            oauth_login=lambda: {"sub": "synthetic-owner"}, http=self.http,
            token_endpoint=ISSUER + "/protocol/openid-connect/token")
        self.runtime = types.SimpleNamespace(root=self.root, state={"project": "clouddrive-demo-synthetic"},
            native_origin=NATIVE, native_http=lambda: self.http, issuer=ISSUER, api=lambda role: api)
        private_write(self.root / "service-credentials.json", {
            "exchange_client_id": "synthetic-exchange", "exchange_client_secret": "test-only-secret"})
        self.sleep = patch.object(probe.time, "sleep", lambda _: None)
        self.sleep.start()
        self.addCleanup(self.sleep.stop)

    def test_synthetic_happy_contract_only(self):
        result = probe.run_preflight(self.runtime)
        self.assertEqual(result["status"], "passed")
        self.assertTrue(all(result["proofs"].values()))
        self.assertEqual(result["limitations"], probe.LIMITATIONS)
        self.assertFalse(result["limitations"]["native_final_commit_cas"])
        self.assertTrue(result["limitations"]["business_durable_operation_gates_required"])
        self.assertEqual(len(self.http.forms), 1)
        exchange = self.http.forms[0][1]
        self.assertEqual(exchange["scope"], "openid")
        self.assertEqual(exchange["audience"], "ocis")
        self.assertNotIn("requested_subject", exchange)
        self.assertNotIn("subject_issuer", exchange)
        self.assertNotIn("DELETE", [c[0] for c in self.http.calls])
        self.assertEqual(self.http.admissions, 2)
        self.assertFalse((self.root / "verified.env").exists())
        serialized = (self.root / "preflight-journal.json").read_text()
        self.assertNotIn("not-for-journal", serialized)
        self.assertNotIn("test-only-secret", serialized)
        self.assertNotIn("Bearer", serialized)
        self.assertNotIn("/data/session", serialized)
        self.assertEqual((self.root / "preflight-journal.json").stat().st_mode & 0o777, 0o600)
        self.assertTrue(self.http.journal_before_io)
        # Native ID obtained from PATCH is durable before subsequent mutation.
        second_post = next(j for j in self.http.journal_before_io if j["operations"][-1]["label"] == "overwrite_admission")
        self.assertEqual(second_post["resource_id"], RID)
        self.assertEqual(second_post["operations"][-1]["target"]["expected_resource_id"], RID)

    def test_successful_rerun_only_reads(self):
        first = probe.run_preflight(self.runtime)
        self.http.calls.clear()
        second = probe.run_preflight(self.runtime)
        self.assertEqual(first["run_id"], second["run_id"])
        self.assertTrue(all(c[0] in ("GET", "PROPFIND") for c in self.http.calls))

    def test_response_loss_blocks_without_replaying_mutation(self):
        self.http.lose_patch_response = True
        with self.assertRaisesRegex(DriverError, "outcome_unknown_create_content"):
            probe.run_preflight(self.runtime)
        journal = private_json(self.root / "preflight-journal.json")
        self.assertEqual(journal["status"], "outcome_unknown")
        self.assertEqual(journal["operations"][-1]["status"], "outcome_unknown")
        self.assertEqual(journal["operations"][-1]["body_sha256"], probe.digest(probe.FIRST))
        calls = len(self.http.calls)
        with self.assertRaisesRegex(DriverError, "previous_probe_incomplete_no_mutation_replay"):
            probe.run_preflight(self.runtime)
        self.assertEqual(len(self.http.calls), calls)
        self.assertEqual(private_json(self.root / "preflight-result.json")["status"], "failed")

    def test_native_server_failure_is_unknown(self):
        self.http.fail_patch_500 = True
        with self.assertRaisesRegex(DriverError, "outcome_unknown"):
            probe.run_preflight(self.runtime)
        self.assertEqual(private_json(self.root / "preflight-journal.json")["status"], "outcome_unknown")

    def test_foreign_signed_location_never_receives_token_or_patch(self):
        self.http.foreign_upload = True
        with self.assertRaisesRegex(DriverError, "admitted_without_trusted_location"):
            probe.run_preflight(self.runtime)
        self.assertFalse(any(c[0] == "PATCH" or "attacker.example" in c[1] for c in self.http.calls))
        self.assertEqual(private_json(self.root / "preflight-journal.json")["status"], "outcome_unknown")

    def test_stale_admission_acceptance_cannot_enable_gate(self):
        self.http.admit_stale = True
        with self.assertRaisesRegex(DriverError, "stale_admission_not_denied_no_patch_sent"):
            probe.run_preflight(self.runtime)
        self.assertEqual(len([c for c in self.http.calls if c[0] == "PATCH"]), 2)
        self.assertFalse(private_json(self.root / "preflight-result.json")["proofs"]["stale_admission_denied"])

    def test_move_identity_change_fails_gate(self):
        self.http.swap_move_identity = True
        with self.assertRaisesRegex(DriverError, "move_changed_identity"):
            probe.run_preflight(self.runtime)
        self.assertEqual(private_json(self.root / "preflight-result.json")["status"], "failed")

    def test_missing_old_revision_after_move_fails_gate(self):
        self.http.drop_history_after_move = True
        with self.assertRaisesRegex(DriverError, "exact_old_revision_missing"):
            probe.run_preflight(self.runtime)
        self.assertFalse(private_json(self.root / "preflight-result.json")["proofs"]["move_retained_history"])

    def test_current_get_stale_ifmatch_ignored_is_observed_not_required(self):
        result = probe.run_preflight(self.runtime)
        self.assertFalse(result["limitations"]["current_get_if_match_enforcement"])
        self.assertEqual(private_json(self.root / "preflight-journal.json")["current_get_stale_if_match_observation"],
                         "ignored_returned_current")

    def test_current_get_stale_ifmatch_denied_does_not_claim_native_cas(self):
        self.http.current_ifmatch_412 = True
        result = probe.run_preflight(self.runtime)
        self.assertFalse(result["limitations"]["current_get_if_match_enforcement"])
        self.assertFalse(result["limitations"]["native_final_commit_cas"])
        self.assertEqual(private_json(self.root / "preflight-journal.json")["current_get_stale_if_match_observation"],
                         "precondition_denied")

    def test_concurrent_preflight_cannot_dispatch_or_replace_marker(self):
        fd = os.open(self.root / "preflight.lock", os.O_CREAT | os.O_RDWR, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(DriverError, "already_running"):
                probe.run_preflight(self.runtime)
            self.assertFalse(self.http.calls)
            self.assertFalse((self.root / "preflight-result.json").exists())
        finally:
            os.close(fd)

    def test_symlink_lock_is_rejected(self):
        target = self.root / "other"
        target.write_text("untouched")
        (self.root / "preflight.lock").symlink_to(target)
        with self.assertRaisesRegex(DriverError, "lock_unavailable"):
            probe.run_preflight(self.runtime)
        self.assertEqual(target.read_text(), "untouched")
        self.assertFalse(self.http.calls)

    def test_dispatched_but_unacknowledged_journal_blocks_before_network(self):
        private_write(self.root / "preflight-journal.json", {"schema": 1,
            "kind": "agent-clouddrive-native-preflight-journal", "project": self.runtime.state["project"],
            "status": "running", "operations": [{"status": "dispatch_pending"}]})
        with self.assertRaisesRegex(DriverError, "previous_probe_incomplete_no_mutation_replay"):
            probe.run_preflight(self.runtime)
        self.assertFalse(self.http.calls)

    def test_unverified_token_claims_alone_cannot_pass(self):
        self.http.graph_status = 401
        with self.assertRaisesRegex(DriverError, "graph_rejected_delegated_user"):
            probe.run_preflight(self.runtime)
        result = private_json(self.root / "preflight-result.json")
        self.assertFalse(result["proofs"]["confidential_exchange_openid"])
        self.assertFalse(any(c[0] in ("POST", "PATCH", "MOVE", "MKCOL") for c in self.http.calls))

    def test_previous_success_marker_invalidated_on_failed_live_recheck(self):
        probe.run_preflight(self.runtime)
        self.http.graph_status = 503
        with self.assertRaises(DriverError):
            probe.run_preflight(self.runtime)
        self.assertEqual(private_json(self.root / "preflight-result.json")["status"], "failed")

    def test_reject_native_url_escape(self):
        for value in ("https://foreign.invalid/dav/spaces/x", "//foreign.invalid/dav/spaces/x",
                      NATIVE + "/dav/spaces/%2e%2e/x", NATIVE + "/dav/spaces/x#frag",
                      "http://user@ocis:9200/dav/spaces/x", NATIVE + "/dav/spaces/x?query=bad",
                      NATIVE + "/dav/spaces/%5csecret"):
            with self.subTest(value=value), self.assertRaises(DriverError):
                probe.safe_native_url(value, NATIVE, prefix="/dav/spaces/")

    def test_native_same_resource_meta_self_collection_is_accepted(self):
        # Regression derived from actual oCIS 8.2.2 shape, not native evidence.
        result = probe.run_preflight(self.runtime)
        self.assertTrue(result["proofs"]["revision_read"])
        journal = private_json(self.root / "preflight-journal.json")
        self_row = journal["last_history_response"]["rows"][0]
        self.assertTrue(self_row["directory"])
        self.assertFalse(self_row["etag_present"])
        self.assertTrue(self_row["path"].endswith("/"))
        self.assertFalse(self_row["path"].endswith("/v/"))

    def test_foreign_revision_href_is_not_read_or_trusted(self):
        self.http.history_foreign = True
        with self.assertRaisesRegex(DriverError, "untrusted_native_endpoint"):
            probe.run_preflight(self.runtime)
        self.assertFalse(any("attacker.example" in c[1] for c in self.http.calls))

    def test_unrelated_meta_collection_is_not_silently_skipped(self):
        self.http.history_wrong_collection = True
        with self.assertRaisesRegex(DriverError, "revision_href_invalid"):
            probe.run_preflight(self.runtime)

    def test_xml_dtd_is_rejected(self):
        with self.assertRaisesRegex(DriverError, "entities_forbidden"):
            probe.xml_rows(b'<!DOCTYPE d [<!ENTITY x "value">]><d:multistatus xmlns:d="DAV:"/>')

    def test_xml_ignores_failed_propstats(self):
        xml = rows([("/path", RID, '"one"', False)]).replace(b" 200 ", b" 404 ")
        self.assertNotIn("resource_id", probe.xml_rows(xml)[0])

    def test_weak_etag_rejected(self):
        with self.assertRaises(DriverError):
            probe.etag('W/"one"')

    def test_wrong_runtime_journal_never_replayed(self):
        private_write(self.root / "preflight-journal.json", {"schema": 1,
            "kind": "agent-clouddrive-native-preflight-journal", "project": "other"})
        with self.assertRaisesRegex(DriverError, "wrong_runtime"):
            probe.run_preflight(self.runtime)
        self.assertFalse(self.http.calls)


if __name__ == "__main__":
    unittest.main(verbosity=2)
