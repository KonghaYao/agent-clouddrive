"""Offline helper/guard/contract tests, never deployed acceptance evidence."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import re
import ssl
import sys
import tempfile
import unittest
from unittest.mock import patch
import uuid
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parent))
from deployed_api_harness import (API, Config, DriverError, Forms, NoRedirect, Transport,
                                  callback_code, protected_credentials, safe_id, validate_url)
from deployed_api_acceptance import (AUTH_ROUTES, CASE_IDS, Suite, assert_batch_result, batch_identities,
                                     main, new_report, parse_scenarios, write_report, search_matches, raw_search_request, raw_search_matches)

ROOT = Path(__file__).resolve().parents[2]


class ConfigurationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "credentials.json"
        self.path.write_text(json.dumps({"owner": {"username": "fixture-owner", "password": "do-not-log-this-password"},
                                       "reader": {"username": "fixture-reader", "password": "different-test-password"}}))
        self.path.chmod(0o600)
        self.env = {"TEST_PUBLIC_URL": "https://drive.example.test", "TEST_OIDC_ISSUER": "https://identity.example.test/realms/fixture",
                    "TEST_CREDENTIALS_FILE": str(self.path)}

    def tearDown(self):
        self.tmp.cleanup()

    def test_protected_credentials(self):
        self.assertEqual(protected_credentials(self.path, True)["owner"]["username"], "fixture-owner")

    def test_optional_raw_search_config_has_separate_protected_key(self):
        key = Path(self.tmp.name) / "search-key"
        key.write_text("private-search-test-key\n")
        key.chmod(0o600)
        env = {**self.env, "TEST_MEILI_URL": "https://search.example.test", "TEST_MEILI_INDEX": "native_e2e",
               "TEST_MEILI_KEY_FILE": str(key)}
        config = Config.from_environment(env, False, True)
        self.assertEqual(config.meili_index, "native_e2e")
        self.assertEqual(config.meili_key, "private-search-test-key")
        rc, report = self.invoke(["--check-only"], env)
        self.assertEqual(rc, 3)
        self.assertNotIn("private-search-test-key", json.dumps(report))
        self.assertNotIn("search.example.test", json.dumps(report))
        key.chmod(0o644)
        with self.assertRaisesRegex(DriverError, "search_key_requires"):
            Config.from_environment(env, False, True)

    def test_optional_raw_search_configuration_rejects_unsafe_destinations(self):
        for extra in ({"TEST_MEILI_URL": "http://external.example.test"},
                      {"TEST_MEILI_INDEX": "native_e2e"},
                      {"TEST_MEILI_URL": "https://search.example.test", "TEST_MEILI_INDEX": "../outside"}):
            with self.assertRaises(DriverError):
                Config.from_environment({**self.env, **extra}, False, True)

    def test_world_readable_credentials_fail_closed(self):
        self.path.chmod(0o644)
        with self.assertRaisesRegex(DriverError, "private_regular_file"):
            protected_credentials(self.path, True)

    def test_symlink_credentials_fail_closed(self):
        link = Path(self.tmp.name) / "link.json"
        link.symlink_to(self.path)
        with self.assertRaises(DriverError):
            protected_credentials(link, True)

    def test_malformed_secret_never_in_exception(self):
        self.path.write_text('{"password":"very-secret-and-invalid-json')
        with self.assertRaises(DriverError) as caught:
            protected_credentials(self.path, True)
        self.assertNotIn("very-secret", str(caught.exception))

    def test_distinct_accounts_required_for_sharing(self):
        data = json.loads(self.path.read_text())
        data["reader"] = data["owner"]
        self.path.write_text(json.dumps(data))
        with self.assertRaisesRegex(DriverError, "distinct_fixture_users_required"):
            protected_credentials(self.path, True)

    def test_reader_not_required_for_nonsharing_subset(self):
        self.path.write_text(json.dumps({"owner": {"username": "fixture", "password": "test-password"}}))
        Config.from_environment(self.env, False, False)
        with self.assertRaises(DriverError):
            Config.from_environment(self.env, False, True)

    def invoke(self, args, env=None):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout), patch.object(Transport, "request", side_effect=AssertionError("network forbidden")):
            rc = main(args, self.env if env is None else env)
        return rc, json.loads(stdout.getvalue())

    def test_check_only_valid_is_not_run_exit_three(self):
        rc, report = self.invoke(["--check-only"])
        self.assertEqual(rc, 3)
        self.assertEqual(report["execution"], "not_run")
        self.assertEqual(report["status"], "not_run")
        self.assertEqual(report["configuration"], "valid")
        self.assertEqual({x["id"] for x in report["cases"]}, set(CASE_IDS))
        self.assertTrue(all(x["status"] == "not_run" for x in report["cases"]))
        self.assertNotIn("do-not-log-this-password", json.dumps(report))
        self.assertNotIn("fixture-owner", json.dumps(report))
        self.assertNotIn("https://identity", json.dumps(report))

    def test_check_only_invalid_is_not_run_exit_two(self):
        rc, report = self.invoke(["--check-only"], {})
        self.assertEqual(rc, 2)
        self.assertEqual(report["status"], "not_run")
        self.assertEqual(report["reason"], "missing_required_configuration")

    def test_actual_run_requires_explicit_write_optin(self):
        rc, report = self.invoke([])
        self.assertEqual(rc, 2)
        self.assertEqual(report["execution"], "not_run")
        self.assertEqual(report["reason"], "explicit_disposable_write_opt_in_required")

    def test_actual_run_requires_separate_offline_consent_optin(self):
        rc, report = self.invoke(["--allow-disposable-writes"])
        self.assertEqual(rc, 2)
        self.assertEqual(report["execution"], "not_run")
        self.assertEqual(report["status"], "not_run")
        self.assertEqual(report["reason"], "explicit_disposable_offline_consent_required")

    def test_output_file_is_private_and_sanitized(self):
        output = Path(self.tmp.name) / "report.json"
        rc, report = self.invoke(["--check-only", "--output", str(output)])
        self.assertEqual(rc, 3)
        self.assertEqual(json.loads(output.read_text()), report)
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)

    def test_output_cannot_overwrite_existing_ordinary_file(self):
        target = Path(self.tmp.name) / "existing.txt"
        target.write_text("original artifact must remain untouched")
        rc, report = self.invoke(["--check-only", "--output", str(target)])
        self.assertEqual(rc, 2)
        self.assertEqual(report["reason"], "report_output_must_be_new")
        self.assertEqual(target.read_text(), "original artifact must remain untouched")

    def test_report_reservation_precedes_actual_login_and_service_writes(self):
        target = Path(self.tmp.name) / "existing.txt"
        target.write_text("unchanged")
        with patch.object(API, "login", side_effect=AssertionError("must not log in")) as login:
            rc, report = self.invoke(["--allow-disposable-writes", "--output", str(target)],
                                    {**self.env, "ALLOW_DISPOSABLE_TEST_OIDC_CONSENT": "yes"})
        self.assertEqual(rc, 2)
        self.assertEqual(report["execution"], "not_run")
        self.assertEqual(report["reason"], "report_output_must_be_new")
        login.assert_not_called()
        self.assertEqual(target.read_text(), "unchanged")

    def test_output_cannot_follow_symlink_to_ordinary_file(self):
        original = Path(self.tmp.name) / "ordinary.txt"
        original.write_text("unchanged")
        target = Path(self.tmp.name) / "linked-report.json"
        target.symlink_to(original)
        rc, report = self.invoke(["--check-only", "--output", str(target)])
        self.assertEqual(rc, 2)
        self.assertTrue(target.is_symlink())
        self.assertEqual(original.read_text(), "unchanged")

    def test_output_cannot_truncate_ordinary_hardlink(self):
        original = Path(self.tmp.name) / "ordinary.txt"
        original.write_text("unchanged")
        target = Path(self.tmp.name) / "hardlinked-report.json"
        os.link(original, target)
        rc, report = self.invoke(["--check-only", "--output", str(target)])
        self.assertEqual(rc, 2)
        self.assertEqual(original.read_text(), "unchanged")
        self.assertEqual(target.read_text(), "unchanged")

    def test_output_cannot_overwrite_credentials(self):
        before = self.path.read_text()
        rc, report = self.invoke(["--check-only", "--output", str(self.path)])
        self.assertEqual(rc, 2)
        self.assertEqual(report["reason"], "report_cannot_replace_credentials")
        self.assertEqual(self.path.read_text(), before)

    def test_output_cannot_overwrite_hardlinked_credentials(self):
        target = Path(self.tmp.name) / "hardlink.json"
        os.link(self.path, target)
        before = self.path.read_text()
        rc, report = self.invoke(["--check-only", "--output", str(target)])
        self.assertEqual(rc, 2)
        self.assertEqual(self.path.read_text(), before)

    def test_unexpected_errors_do_not_dump_exception_details(self):
        with patch.object(API, "login", side_effect=ValueError("secret token URL must not escape")):
            rc, report = self.invoke(["--allow-disposable-writes"], {**self.env, "ALLOW_DISPOSABLE_TEST_OIDC_CONSENT": "yes"})
        self.assertEqual(rc, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["execution"], "executed")
        self.assertNotIn("secret token URL", json.dumps(report))
        self.assertTrue(all(x["status"] == "not_run" for x in report["cases"]))


class URLAndAuthGuardTests(unittest.TestCase):
    def test_tls_required_by_default(self):
        for value in ["http://localhost", "http://127.0.0.1", "http://example.test"]:
            with self.subTest(value=value), self.assertRaises(DriverError):
                validate_url(value)

    def test_loopback_http_is_explicit_and_bounded(self):
        for value in ["http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"]:
            self.assertEqual(validate_url(value, loopback_http=True), value)
        for value in ["http://127.0.0.1.attacker.test", "http://drive.localhost", "http://example.test", "file:///tmp/a"]:
            with self.subTest(value=value), self.assertRaises(DriverError):
                validate_url(value, loopback_http=True)

    def test_url_rejects_auth_query_fragment_traversal_and_invalid_port(self):
        for value in ["https://user:pass@example.test", "https://example.test?a=1", "https://example.test/#x",
                      "https://example.test/a/../b", "https://example.test/%2e%2e/b", "https://example.test:70000",
                      "https://example.test\n", "https://example.test\\x"]:
            with self.subTest(value=value), self.assertRaises(DriverError):
                validate_url(value)

    def test_public_url_requires_origin_not_path(self):
        with self.assertRaises(DriverError):
            validate_url("https://example.test/subpath", base=True)

    def test_no_automatic_redirect_can_forward_bearer(self):
        self.assertIsNone(NoRedirect().redirect_request(None, None, 302, "", {}, "https://attacker.test"))

    def test_pkce_callback_matches_exact_origin_path_and_state(self):
        callback = "https://drive.example.test/auth/callback"
        self.assertEqual(callback_code(callback + "?state=random-state&code=opaque-code", callback, "random-state"), "opaque-code")
        for location in [callback + "?state=wrong&code=x", callback + "?state=random-state&state=random-state&code=x",
                         callback + "?state=random-state&code=x&code=y", callback + "?state=random-state&code=x#error",
                         callback + "?state=random-state&error=access_denied", callback + "/other?state=random-state&code=x",
                         "https://attacker.test/auth/callback?state=random-state&code=x"]:
            with self.subTest(location=location), self.assertRaises(DriverError):
                callback_code(location, callback, "random-state")

    def test_html_form_parser_hidden_fields_and_entities(self):
        forms = Forms()
        forms.feed('<form method="POST" action="https://id.test/realms/x/login-actions/authenticate?a=1&amp;b=2">'
                   '<input name="username"><input name="password" type="password">'
                   '<input name="session_code" type="hidden" value="a&amp;b"></form>')
        self.assertEqual(len(forms.forms), 1)
        self.assertTrue(forms.forms[0]["password"])
        self.assertEqual(forms.forms[0]["fields"], {"session_code": "a&b"})
        self.assertIn("a=1&b=2", forms.forms[0]["action"])

    def test_issuer_guard_rejects_other_realms_origins_and_encoded_paths(self):
        cfg = Config("https://drive.test", "https://id.test/realms/test", {"owner": {"username": "fixture", "password": "password"}})
        api = API(cfg)
        self.assertTrue(api._issuer_url("https://id.test/realms/test/login-actions/authenticate?a=1"))
        for url in ["https://id.test/realms/other/login-actions/authenticate", "https://id.test.attacker/realms/test/x",
                    "https://id.test/realms/test/%2e%2e/x", "https://id.test/realms/test/../x", "https://id.test/realms/test/x#y"]:
            self.assertFalse(api._issuer_url(url))

    def test_uuid_validation_disallows_path_injection(self):
        self.assertEqual(safe_id("00000000-0000-4000-8000-000000000001"), "00000000-0000-4000-8000-000000000001")
        for value in ["../outside", "1234", None, "00000000-0000-4000-8000-000000000001?secret=x"]:
            with self.assertRaises(DriverError):
                safe_id(value)


class RetainedHistoryGuardTests(unittest.TestCase):
    def suite(self):
        # An offline request recorder, not a deployment or identity fixture.
        suite = object.__new__(Suite)
        suite.case = {"checks": []}
        suite.owner = SimpleNamespace(request=lambda *a, **kw: SimpleNamespace(status=404))
        return suite

    def test_owner_lifecycle_denial_never_asserts_history_denied(self):
        suite = self.suite()
        with patch.object(suite.owner, "request", return_value=SimpleNamespace(status=404)) as request:
            suite.denied_current("00000000-0000-4000-8000-000000000001", "deleted_owner")
        paths = [call.args[1] for call in request.call_args_list]
        self.assertEqual(len(paths), 4)
        self.assertFalse(any("/versions" in path or "version_id=" in path for path in paths))
        self.assertTrue(all(c["status"] == "pass" for c in suite.case["checks"]))

    def test_nonowner_denial_includes_versions_and_pinned_history_range(self):
        suite = self.suite()
        with patch.object(suite.owner, "request", return_value=SimpleNamespace(status=403)) as request:
            suite.denied("00000000-0000-4000-8000-000000000001", "disabled_recipient",
                         version="00000000-0000-4000-8000-000000000002")
        paths = [call.args[1] for call in request.call_args_list]
        self.assertEqual(len(paths), 7)
        self.assertTrue(any(path.endswith("/versions") for path in paths))
        self.assertEqual(sum("version_id=" in path for path in paths), 2)
        self.assertEqual(request.call_args_list[-1].kwargs["headers"], {"Range": "bytes=0-3"})

    def test_owner_retention_matches_actual_authorization_contract(self):
        domain = (ROOT / "internal/domain/authorization.go").read_text()
        self.assertIn('if r.Action == ActionReadHistory || r.Action == ActionListTrash {\n\t\t\treturn nil', domain)
        service = (ROOT / "internal/application/service.go").read_text()
        self.assertIn('if vid != "" {\n\t\taction = domain.ActionReadHistory', service)


class SearchVisibilityTests(unittest.TestCase):
    def setUp(self):
        self.expected = {
            "00000000-0000-4000-8000-000000000001": {"version_id": "00000000-0000-4000-8000-000000000003",
                "path": "/tree/direct.txt", "marker": "uniquedirectmarker"},
            "00000000-0000-4000-8000-000000000002": {"version_id": "00000000-0000-4000-8000-000000000004",
                "path": "/tree/nested/deep.txt", "marker": "uniquedeepmarker"},
        }
        self.result = {"items": [{"file_id": fid, "returned_version_id": row["version_id"],
            "current_version_id": row["version_id"], "artifact_set_id": str(uuid.uuid4()), "path": row["path"],
            "stale": False, "newer_version_status": "ready", "completeness": {"status": "ready"},
            "snippet": "synthetic " + row["marker"]} for fid, row in self.expected.items()], "next_cursor": None}

    def test_exact_current_files_versions_paths_and_markers_match(self):
        self.assertTrue(search_matches(self.result, self.expected))
        for row in self.result["items"]:
            self.expected[row["file_id"]]["artifact_set_id"] = row["artifact_set_id"]
        self.assertTrue(search_matches(self.result, self.expected))

    def test_missing_or_extra_files_and_cursor_do_not_pass(self):
        for edit in (lambda r: r["items"].pop(), lambda r: r.update(next_cursor="more"),
                     lambda r: r["items"].append({**r["items"][0], "file_id": str(uuid.uuid4())})):
            result = copy.deepcopy(self.result)
            edit(result)
            self.assertFalse(search_matches(result, self.expected))

    def test_stale_wrong_path_version_artifact_or_unready_cannot_pass(self):
        edits = {"returned_version_id": str(uuid.uuid4()), "current_version_id": str(uuid.uuid4()),
                 "path": "/old-tree/direct.txt", "stale": True, "newer_version_status": "processing",
                 "completeness": {"status": "partial"}, "snippet": "wrong synthetic content"}
        for key, value in edits.items():
            result = copy.deepcopy(self.result)
            result["items"][0][key] = value
            self.assertFalse(search_matches(result, self.expected), key)
        expected = copy.deepcopy(self.expected)
        expected[self.result["items"][0]["file_id"]]["artifact_set_id"] = str(uuid.uuid4())
        self.assertFalse(search_matches(self.result, expected))

    def test_every_chunk_must_match_not_only_one_lucky_hit(self):
        result = copy.deepcopy(self.result)
        result["items"].append({**result["items"][0], "path": "/old-path"})
        self.assertFalse(search_matches(result, self.expected))
        result["items"][-1] = {**result["items"][0], "artifact_set_id": str(uuid.uuid4())}
        self.assertFalse(search_matches(result, self.expected))

    def test_invalid_search_shapes_raise_safe_diagnostic(self):
        for result in ({}, {"items": None}, {"items": [None]}):
            with self.assertRaises(DriverError):
                search_matches(result, self.expected)

    def suite(self, results):
        suite = object.__new__(Suite)
        suite.case = {"checks": []}
        suite.owner = SimpleNamespace(json=lambda *a, **kw: None)
        return suite, patch.object(suite.owner, "json", side_effect=results)

    def test_poll_waits_for_actual_matching_result(self):
        suite, requests = self.suite([{"items": []}, self.result])
        with requests as calls, patch("deployed_api_acceptance.time.sleep") as sleep:
            got = suite.wait_search("commonmarker", self.expected, "ready")
        self.assertEqual(got, self.result)
        self.assertEqual(calls.call_count, 2)
        self.assertEqual(sleep.call_count, 1)
        self.assertEqual(suite.case["checks"], [{"name": "ready", "status": "pass"}])
        self.assertEqual(calls.call_args.args, ("POST", "/v1/search", {"query": "commonmarker", "limit": 100}))

    def test_missing_worker_results_fail_at_deadline_never_pass(self):
        suite, requests = self.suite([{"items": []}])
        with requests, patch("deployed_api_acceptance.time.monotonic", side_effect=[100, 221]), \
                patch("deployed_api_acceptance.time.sleep") as sleep:
            with self.assertRaises(DriverError):
                suite.wait_search("commonmarker", self.expected, "publication_required")
        self.assertEqual(suite.case["checks"], [{"name": "publication_required", "status": "fail"}])
        sleep.assert_not_called()

    def test_deleted_search_leak_fails_immediately_without_wait(self):
        suite, requests = self.suite([self.result])
        with requests, patch("deployed_api_acceptance.time.sleep") as sleep:
            with self.assertRaises(DriverError):
                suite.search_hidden("commonmarker", "must_hide_immediately")
        self.assertEqual(suite.case["checks"][0]["status"], "fail")
        sleep.assert_not_called()

    def test_search_fields_match_actual_application_contract(self):
        source = (ROOT / "internal/application/query.go").read_text()
        for field in ("file_id", "path", "returned_version_id", "current_version_id", "artifact_set_id",
                      "stale", "newer_version_status", "snippet", "completeness"):
            self.assertIn('"' + field + '":', source)
        self.assertIn('"path": str(f, "path")', source)


class RawIndexVisibilityTests(unittest.TestCase):
    def setUp(self):
        self.expected = {str(uuid.uuid4()): {"version_id": str(uuid.uuid4()), "artifact_set_id": str(uuid.uuid4()),
                                           "path": "/synthetic/new-path.txt"} for _ in range(2)}
        self.response = {"hits": [{"file_id": fid, "version_id": wanted["version_id"],
            "artifact_set_id": wanted["artifact_set_id"], "source_path": wanted["path"]}
            for fid, wanted in self.expected.items()]}

    def test_filter_scopes_only_created_files_and_artifact_ids(self):
        request = raw_search_request(self.expected)
        self.assertEqual(request["q"], "")
        self.assertEqual(request["limit"], 100)
        self.assertEqual(request["attributesToRetrieve"], ["file_id", "version_id", "artifact_set_id", "source_path"])
        for fid, wanted in self.expected.items():
            self.assertIn('file_id = "' + fid + '"', request["filter"])
            self.assertIn('artifact_set_id = "' + wanted["artifact_set_id"] + '"', request["filter"])
        with self.assertRaises(DriverError):
            raw_search_request({'" OR owner_id != "': next(iter(self.expected.values()))})

    def test_source_path_version_and_artifact_must_all_match(self):
        self.assertTrue(raw_search_matches(self.response, self.expected))
        for field in ("source_path", "version_id", "artifact_set_id"):
            result = copy.deepcopy(self.response)
            result["hits"][0][field] = "wrong"
            self.assertFalse(raw_search_matches(result, self.expected))
        for result in ({"hits": []}, {"hits": self.response["hits"][:1]}, {"hits": self.response["hits"] * 50}):
            self.assertFalse(raw_search_matches(result, self.expected))

    def test_raw_search_never_uses_application_bearer(self):
        suite = object.__new__(Suite)
        suite.case = {"checks": []}
        suite.config = SimpleNamespace(meili_url="https://search.test", meili_index="native_e2e", meili_key="raw-index-test-key")
        suite.owner = SimpleNamespace(access="must-not-use-application-bearer")
        suite.search_http = SimpleNamespace(request=lambda *a, **kw: None)
        response = SimpleNamespace(status=200, json=lambda: self.response)
        with patch.object(suite.search_http, "request", return_value=response) as call:
            suite.wait_raw_search(self.expected, "raw_path")
        self.assertEqual(call.call_args.args[0:2], ("POST", "https://search.test/indexes/native_e2e/search"))
        self.assertEqual(call.call_args.args[3], {"Authorization": "Bearer raw-index-test-key"})
        self.assertNotIn("must-not-use-application-bearer", str(call.call_args))
        suite.config.meili_key = ""
        with patch.object(suite.search_http, "request", return_value=response) as call:
            suite.wait_raw_search(self.expected, "raw_path_no_auth")
        self.assertEqual(call.call_args.args[3], {})

    def test_missing_optional_raw_client_records_no_false_pass(self):
        suite = object.__new__(Suite)
        suite.case = {"checks": []}
        suite.search_http = None
        suite.wait_raw_search(self.expected, "must_not_be_claimed")
        self.assertEqual(suite.case["checks"], [])

    def test_unavailable_raw_index_fails_closed(self):
        suite = object.__new__(Suite)
        suite.case = {"checks": []}
        suite.config = SimpleNamespace(meili_url="https://search.test", meili_index="native_e2e", meili_key="")
        suite.search_http = SimpleNamespace(request=lambda *args: SimpleNamespace(status=503))
        with self.assertRaisesRegex(DriverError, "raw_search_http_failure_503"):
            suite.wait_raw_search(self.expected, "raw_index_required")
        self.assertEqual(suite.case["checks"], [])


class BatchAndCoverageTests(unittest.TestCase):
    @staticmethod
    def valid_batch():
        items = []
        for n in range(100):
            row = {"item_id": "item-%03d" % n, "status": "succeeded", "result": {"value": {
                "state": "committed", "operation_id": str(uuid.uuid4()), "file_id": str(uuid.uuid4())}}}
            if n == 50:
                row.update(status="failed", error={"code": "PATH_CONFLICT"})
            items.append(row)
        return {"state": "completed", "items": items, "succeeded": [r for r in items if r["status"] == "succeeded"],
                "failed": [r for r in items if r["status"] == "failed"], "pending": []}

    def test_exact_100_one_conflict_contract(self):
        result = self.valid_batch()
        assert_batch_result(result)
        self.assertEqual(len(batch_identities(result)), 99)

    def test_rejects_missing_duplicate_pending_and_wrong_failure(self):
        for change in (lambda x: x["items"].pop(), lambda x: x["items"][0].update(item_id="item-001"),
                       lambda x: x.update(pending=[{}]), lambda x: x["failed"][0].update(error={"code": "ACCESS_DENIED"}),
                       lambda x: x["succeeded"][0].update(status="running")):
            result = self.valid_batch()
            change(result)
            with self.assertRaises(DriverError):
                assert_batch_result(result)

    def test_rejects_fake_success_with_uncommitted_or_duplicate_file(self):
        result = self.valid_batch()
        result["succeeded"][0]["result"]["value"]["state"] = "outcome_unknown"
        with self.assertRaises(DriverError):
            batch_identities(result)
        result = self.valid_batch()
        result["succeeded"][0]["result"]["value"]["file_id"] = result["succeeded"][1]["result"]["value"]["file_id"]
        with self.assertRaises(DriverError):
            batch_identities(result)

    def test_all_declared_cases_start_not_run(self):
        report = new_report(parse_scenarios("all"))
        self.assertEqual(len(report["cases"]), 5)
        self.assertTrue(all(c["status"] == "not_run" and not c["checks"] for c in report["cases"]))

    def test_select_subset_and_reject_ambiguous_selections(self):
        self.assertEqual(parse_scenarios("subtree,batch100"), ["subtree", "batch100"])
        for value in ("", "all,subtree", "subtree,subtree", "unknown"):
            with self.assertRaises(DriverError):
                parse_scenarios(value)

    def test_anonymous_inventory_matches_actual_bearer_routes(self):
        source = (ROOT / "internal/transport/http.go").read_text()
        actual = set(re.findall(r'\{"(GET|POST|PATCH|DELETE|HEAD)", "(/v1/[^"]+)"', source))
        actual |= set(re.findall(r'h\.(GET|POST|PATCH|DELETE|HEAD)\("(/v1/[^"]+)"', source))
        actual -= {("GET", "/v1/delegation/callback"), ("GET", "/v1/download/:token")}
        self.assertEqual(set(AUTH_ROUTES), actual)
        self.assertEqual(len(AUTH_ROUTES), len(set(AUTH_ROUTES)))

    def test_actual_contract_fields_are_present_in_server(self):
        batch = (ROOT / "internal/application/batch.go").read_text()
        for value in ('`json:"item_id"`', '`json:"command"`', '`json:"depends_on"`', '`json:"input"`',
                      'len(commands) > 100', '"succeeded": succeeded', '"failed": failed', '"pending": pending'):
            self.assertIn(value, batch)
        mutations = (ROOT / "internal/application/mutations.go").read_text()
        for value in ('"expected_parent_revision"', '"expected_metadata_revision"', '"target_parent_id"',
                      '"target_name"', '"file_id": str(op, "file_id")', '"state": str(op, "state")'):
            self.assertIn(value, mutations)


if __name__ == "__main__":
    unittest.main()
