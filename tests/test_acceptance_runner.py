"""Runner validation tests, not live product/browser acceptance."""
import importlib.util
import ast
import json
from pathlib import Path
import re
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("acceptance_runner", Path(__file__).resolve().parents[1] / "scripts" / "acceptance.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


def report(status="pass", execution="executed", statuses=None):
    return {"schema_version": 1, "status": status, "execution": execution, "cases": [{"id": f"case-{i}", "status": s} for i, s in enumerate(statuses or [status])]}


class RunnerValidation(unittest.TestCase):
    def test_suite_credentials_are_separated_without_mutating_parent_environment(self):
        env = {"TEST_CREDENTIALS_FILE": "legacy", "TEST_BROWSER_CREDENTIALS_FILE": "browser-private", "TEST_API_CREDENTIALS_FILE": "api-private"}
        self.assertEqual(runner.suite_environment("browser", env)["TEST_CREDENTIALS_FILE"], "browser-private")
        self.assertEqual(runner.suite_environment("deployed-api", env)["TEST_CREDENTIALS_FILE"], "api-private")
        self.assertEqual(runner.suite_environment("uppy-10gib", env)["TEST_CREDENTIALS_FILE"], "legacy")
        self.assertEqual(env["TEST_CREDENTIALS_FILE"], "legacy")

    def test_single_suite_legacy_environment_is_preserved(self):
        env = {"TEST_CREDENTIALS_FILE": "one-private-file"}
        self.assertEqual(runner.suite_environment("browser", env), env)

    def test_browser_registry_matches_imported_pure_case_inventory(self):
        # Import guard guarantees this prints constants only: no main(), browser,
        # OIDC configuration, network or credential reads.
        result = subprocess.run(["node", "--input-type=module", "-e", "import {CASE_IDS} from './web/scripts/browser-acceptance.mjs'; process.stdout.write(JSON.stringify(CASE_IDS));"], cwd=runner.ROOT, check=True, capture_output=True, text=True, timeout=10)
        self.assertEqual(runner.SUITES["browser"]["case_ids"], json.loads(result.stdout))

    def test_api_registry_matches_literal_inventory(self):
        tree = ast.parse((runner.ROOT / "tests/integration/deployed_api_acceptance.py").read_text())
        assignment = next(n for n in tree.body if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == "CASE_IDS" for t in n.targets))
        self.assertEqual(runner.SUITES["deployed-api"]["case_ids"], list(ast.literal_eval(assignment.value)))

    def test_large_registry_matches_literal_inventory_without_importing_entrypoint(self):
        source = (runner.ROOT / "web/scripts/large-upload-acceptance.mjs").read_text()
        inventory = re.search(r"const CASES = \[(.*?)\n\];", source, re.S)
        self.assertIsNotNone(inventory)
        ids = re.findall(r"\['([a-z0-9_]+)',", inventory.group(1))
        self.assertEqual(runner.SUITES["uppy-10gib"]["case_ids"], ids)

    def test_actual_success_is_accepted(self):
        self.assertEqual(runner.assess_report(report(), checking=False, returncode=0), ("pass", []))

    def test_check_only_can_never_pass(self):
        status, errors = runner.assess_report(report(), checking=True, returncode=0)
        self.assertEqual(status, "fail")
        self.assertTrue(errors)

    def test_valid_check_remains_not_run(self):
        self.assertEqual(runner.assess_report(report("not_run", "not_run"), checking=True, returncode=3), ("not_run", []))

    def test_missing_setup_remains_not_run(self):
        self.assertEqual(runner.assess_report(report("not_run", "not_run"), checking=True, returncode=2), ("not_run", []))

    def test_empty_cases_fail(self):
        value = report(); value["cases"] = []
        self.assertEqual(runner.assess_report(value, checking=False, returncode=0)[0], "fail")

    def test_skipped_case_cannot_pass(self):
        self.assertEqual(runner.assess_report(report(statuses=["pass", "not_run"]), checking=False, returncode=0)[0], "fail")

    def test_duplicate_ids_fail(self):
        value = report(); value["cases"] *= 2
        self.assertEqual(runner.assess_report(value, checking=False, returncode=0)[0], "fail")

    def test_subprocess_failure_overrules_pass(self):
        self.assertEqual(runner.assess_report(report(), checking=False, returncode=1)[0], "fail")

    def test_unexecuted_pass_fails(self):
        self.assertEqual(runner.assess_report(report(execution="not_run"), checking=False, returncode=0)[0], "fail")

    def test_false_zero_exit_fails(self):
        self.assertEqual(runner.assess_report(report("not_run", "not_run"), checking=False, returncode=0)[0], "fail")

    def test_missing_report_fails(self):
        self.assertEqual(runner.assess_report(None, checking=False, returncode=0)[0], "fail")

    def test_unknown_status_fails(self):
        self.assertEqual(runner.assess_report(report("skip"), checking=False, returncode=0)[0], "fail")

    def test_missing_required_cases_fail(self):
        self.assertEqual(runner.assess_report(report(), checking=False, returncode=0, expected_ids=["case-0", "case-1"])[0], "fail")

    def test_smoke_cannot_impersonate_large_suite(self):
        value = report(); value["suite"] = "uppy-small-smoke"; value["accepts_10gib"] = False
        self.assertEqual(runner.assess_report(value, checking=False, returncode=0, expected_suite="uppy-10gib")[0], "fail")

    def test_large_suite_must_prove_eligibility(self):
        value = report(); value["suite"] = "uppy-10gib"; value["accepts_10gib"] = False
        self.assertEqual(runner.assess_report(value, checking=False, returncode=0, expected_suite="uppy-10gib")[0], "fail")

    def test_unexpected_case_is_rejected(self):
        self.assertEqual(runner.assess_report(report(), checking=False, returncode=0, expected_ids=[])[0], "fail")


if __name__ == "__main__":
    unittest.main()
