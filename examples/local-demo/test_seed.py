"""OFFLINE guard/journal/protocol decision tests, NOT native/demo acceptance."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import re
import socket
import sys
import tempfile
import unittest
from unittest.mock import patch
import uuid

sys.path.insert(0, str(Path(__file__).resolve().parent))
import demo_support as support
import seed
from deployed_api_harness import Config, DriverError, Response, validate_url


def uid(): return str(uuid.uuid4())


class OfflineFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name); self.root.chmod(0o700)
        self.creds = {r: {"username": r + "-" + "a" * 16, "password": "secret-test-password-" + r} for r in ("owner", "reader")}
        self.state = {"schema": 1, "kind": "agent-clouddrive-local-demo", "project": "demo-test-abc123", "public_url": "http://drive.localhost:8080", "storage_mode": "local"}
        support.private_write(self.root / "demo-state.json", self.state)
        support.private_write(self.root / "credentials.json", self.creds)
        self.runtime = support.load_runtime(self.root, True)

    def tearDown(self): self.temp.cleanup()

    def fixture(self): return seed.Seed(self.runtime, timeout=0)

    def directory(self, fixture):
        parent = uid()
        fixture.journal["steps"]["directory"] = {"request": {"name": fixture.journal["folder_name"], "parent_id": "", "idempotency_key": "demo-" + "b" * 32}, "complete": True, "file_id": parent}
        fixture.save()
        return parent

    def test_runtime_guard(self):
        self.assertEqual(self.runtime.issuer, "http://drive.localhost:8080/identity/realms/clouddrive")
        self.assertNotIn("secret-test", repr(self.runtime))
        self.assertNotIn("secret-test", repr(self.runtime.config))

    def test_non_demo_credentials_rejected(self):
        self.creds["owner"]["username"] = "real-person"
        support.private_write(self.root / "credentials.json", self.creds)
        with self.assertRaisesRegex(DriverError, "synthetic_demo_user_required"):
            support.load_runtime(self.root)

    def test_private_files_fail_closed(self):
        path = self.root / "demo-state.json"; path.chmod(0o644)
        with self.assertRaises(DriverError): support.load_runtime(self.root)

    def test_private_json_symlink_and_hardlink_rejected(self):
        target = self.root / "demo-state.json"
        link = self.root / "link.json"; link.symlink_to(target)
        with self.assertRaises(DriverError): support.private_json(link)
        link.unlink(); os.link(target, link)
        with self.assertRaises(DriverError): support.private_json(link)

    def test_insecure_runtime_rejected(self):
        self.root.chmod(0o755)
        with self.assertRaises(DriverError): support.load_runtime(self.root)

    def test_external_public_origin_rejected(self):
        for endpoint in ("http://example.org:8080", "http://drive.localhost:8080@evil.test:8080", "http://drive.localhost:8080/?x=secret", "http://drive.localhost:8080/path"):
            self.state["public_url"] = endpoint
            support.private_write(self.root / "demo-state.json", self.state)
            with self.assertRaises(DriverError): support.load_runtime(self.root)

    def test_original_harness_http_policy_unchanged(self):
        with self.assertRaisesRegex(DriverError, "https_required"):
            validate_url("http://drive.localhost:8080", loopback_http=True)

    def test_journal_is_private_secret_free_and_bound(self):
        fixture = self.fixture()
        data = (self.root / "seed-journal.json").read_text()
        self.assertNotIn("secret-test", data)
        self.assertNotIn("access_token", data)
        self.assertEqual((self.root / "seed-journal.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.fixture().journal, fixture.journal)
        self.runtime.state["project"] = "different-project"
        with self.assertRaisesRegex(DriverError, "unbound"): self.fixture()

    def test_journal_rejects_unknown_secret_fields(self):
        fixture = self.fixture(); corrupted = copy.deepcopy(fixture.journal)
        corrupted["access_token"] = "do-not-print-this"
        with self.assertRaises(DriverError) as error:
            seed.schema_journal(corrupted, fixture.journal["binding"])
        self.assertNotIn("do-not-print", str(error.exception))

    def test_passed_without_completed_steps_is_not_ready(self):
        fixture = self.fixture(); fixture.journal["status"] = "passed"
        with self.assertRaisesRegex(DriverError, "passed_seed_journal_incomplete"): fixture.save()

    def test_journal_rejects_changed_marker_or_schema(self):
        fixture = self.fixture()
        for changes in ({"schema": 999}, {"marker": "x"}, {"folder_name": "someone-elses-folder"}):
            with self.assertRaises(DriverError): seed.schema_journal({**fixture.journal, **changes}, fixture.journal["binding"])

    def test_source_generation_deterministic_and_pdf_valid_structure(self):
        fixture = self.fixture(); a = seed.sample_sources(fixture.journal["marker"])
        self.assertEqual(a, seed.sample_sources(fixture.journal["marker"]))
        self.assertEqual(len(a), 4); self.assertNotEqual(a[0]["bytes"], a[1]["bytes"])
        pdf = a[-1]["bytes"]
        xref = int(re.search(rb"startxref\n(\d+)", pdf)[1])
        self.assertEqual(pdf[xref:xref + 4], b"xref")
        self.assertIn(fixture.journal["marker"].encode(), pdf)
        for offset in re.findall(rb"(\d{10}) 00000 n", pdf):
            self.assertRegex(pdf[int(offset):int(offset) + 8], rb"[1-5] 0 obj")
        with self.assertRaises(DriverError): seed.make_pdf("evil)Tj(secrets")

    def test_generated_samples_private_outside_source_and_no_clobber(self):
        fixture = self.fixture(); fixture.write_samples()
        for path in (self.root / "samples").iterdir(): self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        fixture.write_samples()
        changed = self.root / "samples" / fixture.sources[0]["local"]
        changed.write_text("user change")
        with self.assertRaisesRegex(DriverError, "modified_no_overwrite"): fixture.write_samples()
        self.assertEqual(changed.read_text(), "user change")

    def test_samples_directory_symlink_rejected(self):
        elsewhere = self.root / "elsewhere"; elsewhere.mkdir()
        (self.root / "samples").symlink_to(elsewhere, target_is_directory=True)
        with self.assertRaisesRegex(DriverError, "unsafe_samples_directory"): self.fixture().write_samples()

    def test_check_does_not_create_missing_journal(self):
        with self.assertRaisesRegex(DriverError, "journal_missing"): seed.Seed(self.runtime, check_only=True)
        self.assertFalse((self.root / "seed-journal.json").exists())

    def test_upload_intent_written_before_network(self):
        fixture = self.fixture(); parent = self.directory(fixture); source = fixture.sources[0]
        class RejectNetwork:
            def json(inner, method, path, body=None):
                disk = support.private_json(fixture.path)
                self.assertEqual(disk["steps"]["notes-v1"]["request"], body)
                raise DriverError("network_or_tls_failure_outcome_unconfirmed")
        fixture.owner = RejectNetwork()
        with self.assertRaises(DriverError): fixture.upload(source, parent)
        saved = fixture.journal["steps"]["notes-v1"]["request"].copy()
        with self.assertRaises(DriverError): fixture.upload(source, parent)
        self.assertEqual(saved, fixture.journal["steps"]["notes-v1"]["request"])

    def test_saved_upload_response_is_verified_before_any_patch(self):
        fixture = self.fixture(); source = fixture.sources[0]; fid, vid, upid = uid(), uid(), uid()
        class Committed:
            def json(inner, method, path, body=None):
                if path == "/v1/uploads": return {"id": upid, "file_id": fid, "malicious_token": "dont-persist"}
                return {"file_id": fid, "version_id": vid, "status": "saved", "refresh_token": "dont-persist"}
            def request(inner, *args, **kwargs): self.fail("no PATCH needed for committed replay")
        fixture.owner = Committed(); fixture.upload(source, self.directory(fixture))
        self.assertTrue(fixture.journal["steps"]["notes-v1"]["complete"])
        self.assertNotIn("dont-persist", fixture.path.read_text())
        fixture.upload(source, uid())  # no API or new version

    def test_outcome_unknown_does_not_send_patch(self):
        fixture = self.fixture()
        class Unknown:
            def json(inner, method, path, body=None):
                if path == "/v1/uploads": return {"id": uid(), "file_id": uid()}
                return {"status": "outcome_unknown"}
            def request(inner, *args, **kwargs): self.fail("must not PATCH unknown native state")
        fixture.owner = Unknown()
        parent = self.directory(fixture)
        with self.assertRaisesRegex(DriverError, "pending_no_blind_retry"): fixture.upload(fixture.sources[0], parent)
        self.assertFalse(fixture.journal["steps"]["notes-v1"]["complete"])

    def test_known_tus_offset_resumes_only_remaining_bytes(self):
        fixture = self.fixture(); fixture.timeout = 10
        parent = self.directory(fixture); source = fixture.sources[0]
        fid, vid, upid = uid(), uid(), uid(); calls = []
        class Resume:
            finalized = 0
            def json(inner, method, path, body=None):
                if path == "/v1/uploads": return {"id": upid, "file_id": fid}
                inner.finalized += 1
                if inner.finalized == 1: return {"status": "uploading"}
                return {"status": "saved", "file_id": fid, "version_id": vid}
            def request(inner, method, path, body=None, headers=None):
                calls.append((method, body, headers))
                if method == "HEAD": return Response(200, {"Upload-Length": str(len(source["bytes"])), "Upload-Offset": "7"}, b"")
                self.assertEqual(body, source["bytes"][7:]); self.assertEqual(headers["Upload-Offset"], "7")
                return Response(204, {"Upload-Offset": str(len(source["bytes"]))}, b"")
        fixture.owner = Resume()
        with patch.object(seed.time, "sleep"): fixture.upload(source, parent)
        self.assertEqual([c[0] for c in calls], ["HEAD", "PATCH"])
        self.assertTrue(fixture.journal["steps"]["notes-v1"]["complete"])

    def test_corrupted_journal_cannot_adopt_unrelated_parent(self):
        fixture = self.fixture(); parent = self.directory(fixture); source = fixture.sources[0]
        fixture.new_step(source["step"], {"name": source["name"], "parent_id": parent,
            "size_bytes": len(source["bytes"]), "content_type": source["type"], "idempotency_key": "demo-" + "c" * 32}, source["bytes"])
        fixture.journal["steps"]["notes-v1"]["request"]["parent_id"] = uid()
        with self.assertRaisesRegex(DriverError, "parent_identity"): fixture.save()

    def test_light_ready_does_not_mutate_or_recreate_samples(self):
        fixture = self.fixture(); parent = self.directory(fixture)
        for index, source in enumerate(fixture.sources):
            body = {"name": source["name"], "parent_id": parent, "size_bytes": len(source["bytes"]),
                    "content_type": source["type"], "idempotency_key": "demo-" + str(index + 1) * 32}
            previous = fixture.journal["steps"].get(source["previous"])
            if previous:
                body.update(file_id=previous["file_id"], expected_version_id=previous["version_id"], expected_metadata_revision=1)
            step = fixture.new_step(source["step"], body, source["bytes"])
            step.update(complete=True, file_id=previous["file_id"] if previous else uid(), version_id=uid(), upload_id=uid())
            fixture.save()
        fixture.journal["status"] = "passed"; fixture.save()
        identities = {role: uid() for role in ("owner", "reader")}; paths = []
        class ReadOnly:
            def __init__(inner, role): inner.role = role
            def oauth_login(inner): return {"sub": inner.role}
            def json(inner, method, path):
                paths.append((method, path)); self.assertEqual((method, path), ("GET", "/v1/me"))
                return {"id": identities[inner.role]}
        with patch.object(self.runtime, "api", side_effect=ReadOnly):
            result = seed.ready(self.runtime)
        self.assertFalse(result["sample_contents_rechecked"])
        self.assertEqual(paths, [("GET", "/v1/me"), ("GET", "/v1/me")])
        self.assertFalse((self.root / "samples").exists())

    def test_current_user_edit_refuses_next_version(self):
        fixture = self.fixture(); fid, vid = uid(), uid()
        fixture.journal["steps"]["notes-v1"] = {"file_id": fid, "version_id": vid, "complete": True}
        class Changed:
            def json(inner, method, path, body=None): return {"current_version_id": uid(), "name": "验收说明.md"}
        fixture.owner = Changed()
        with self.assertRaisesRegex(DriverError, "changed_no_automatic_overwrite"): fixture.upload(fixture.sources[1], uid())
        self.assertNotIn("notes-v2", fixture.journal["steps"])

    def test_redacted_unexpected_failures(self):
        out = io.StringIO()
        with patch.object(seed, "load_runtime", side_effect=ValueError("super-secret-password")), contextlib.redirect_stdout(out):
            self.assertEqual(seed.main(["--runtime", str(self.root)]), 2)
        self.assertNotIn("super-secret", out.getvalue())
        self.assertEqual(json.loads(out.getvalue())["status"], "failed")

    def test_untrusted_demo_request_fails_before_io(self):
        transport = support.DemoTransport(self.runtime)
        for url in ("http://evil.test/token", "http://drive.localhost:8080@evil.test/", "http://drive.localhost:8080/../secrets", "http://drive.localhost:8080/%2e%2e/secrets", "http://drive.localhost:8080/%252e%252e/secrets", "file:///etc/passwd"):
            with self.assertRaisesRegex(DriverError, "untrusted_demo_endpoint"): transport.request("GET", url)

    def test_public_resolution_and_mixed_answer_rejected(self):
        def answers(*addresses): return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (ip, 8080)) for ip in addresses]
        for ips in (("8.8.8.8",), ("127.0.0.1", "8.8.8.8"), ("169.254.169.254",), ("0.0.0.0",)):
            with patch.object(socket, "getaddrinfo", return_value=answers(*ips)):
                with self.assertRaises(DriverError): support.pinned_addresses("drive.localhost", 8080, True)
        with patch.object(socket, "getaddrinfo", return_value=answers("172.20.0.4")):
            with self.assertRaises(DriverError): support.pinned_addresses("drive.localhost", 8080, False)
            self.assertEqual(support.pinned_addresses("drive.localhost", 8080, True), ["172.20.0.4"])

    def test_search_hit_binds_published_artifact_not_merely_valid_uuid(self):
        fid, vid, aid = uid(), uid(), uid()
        metadata = {"current_version_id": vid, "search_artifact_set_id": aid, "path": "/demo/sample.txt"}
        row = {"file_id": fid, "returned_version_id": vid, "current_version_id": vid, "artifact_set_id": aid,
               "path": metadata["path"], "stale": False, "newer_version_status": "ready",
               "completeness": {"status": "ready"}, "snippet": "unique-marker"}
        self.assertTrue(seed.published_hit_matches(row, vid, metadata, "unique-marker"))
        for change in ({"artifact_set_id": uid()}, {"current_version_id": uid()}, {"returned_version_id": uid()},
                       {"completeness": None}, {"path": "/wrong"}, {"stale": True}, {"newer_version_status": "partial"}):
            self.assertFalse(seed.published_hit_matches({**row, **change}, vid, metadata, "unique-marker"))
        self.assertFalse(seed.published_hit_matches(row, vid, {**metadata, "search_artifact_set_id": None}, "unique-marker"))

    def test_tls_native_proof_requires_explicit_ca_and_loopback(self):
        self.runtime.container_network = False
        self.runtime.native_origin = "https://example.org:59201"
        with self.assertRaisesRegex(DriverError, "loopback_tls"): self.runtime.native_http()
        self.runtime.native_origin = "https://localhost:59201"
        with self.assertRaisesRegex(DriverError, "loopback_tls"): self.runtime.native_http()


if __name__ == "__main__": unittest.main(verbosity=2)
