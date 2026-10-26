"""Fail-closed validator tests; their acceptance evidence is entirely synthetic."""
import copy
import importlib.util
import hashlib
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("readiness", ROOT / "scripts/release-readiness.py")
readiness = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(readiness)


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        proof = self.root / "proof.txt"
        proof.write_text("synthetic validator fixture, not acceptance evidence")
        self.manifest = {
            "schema_version": 1,
            "release_decision": "ready",
            "evidence": {
                "proof": {
                    "path": "proof.txt",
                    "level": "native_end_to_end",
                    "scope": "validator test only",
                    "sha256": hashlib.sha256(proof.read_bytes()).hexdigest(),
                }
            },
            "source_verification": {
                "status": "pass",
                "evidence": ["proof"],
                "tree_sha256": readiness.source_digest(self.root),
            },
            "gates": [
                {
                    "id": gate_id,
                    "title": "fixture",
                    "requirement": "fixture",
                    "verified_scope": "fixture",
                    "verification_level": "native_end_to_end",
                    "status": "pass",
                    "evidence": ["proof"],
                    "remaining": [],
                }
                for gate_id in sorted(readiness.REQUIRED_IDS)
            ],
        }

    def result(self):
        return readiness.inspect(self.root, self.manifest)

    def test_consistent_fixture(self):
        self.assertTrue(self.result()["ready"])

    def test_missing_required_gate(self):
        self.manifest["gates"].pop()
        self.assertTrue(self.result()["errors"])

    def test_duplicate_gate(self):
        self.manifest["gates"].append(copy.deepcopy(self.manifest["gates"][0]))
        self.assertTrue(self.result()["errors"])

    def test_unknown_gate(self):
        self.manifest["gates"][0]["id"] = "WAIVER"
        self.assertTrue(self.result()["errors"])

    def test_partial_blocks(self):
        self.manifest["release_decision"] = "not_ready"
        self.manifest["gates"][0].update(status="partial", remaining=["not executed"])
        self.assertFalse(self.result()["ready"])
        self.assertFalse(self.result()["errors"])

    def test_missing_file(self):
        (self.root / "proof.txt").unlink()
        self.assertTrue(self.result()["errors"])

    def test_changed_evidence(self):
        (self.root / "proof.txt").write_text("changed")
        self.assertTrue(self.result()["errors"])

    def test_malformed_evidence(self):
        self.manifest["evidence"]["proof"] = "invalid"
        self.assertTrue(self.result()["errors"])

    def test_contract_not_native(self):
        self.manifest["evidence"]["proof"]["level"] = "protocol_contract"
        self.assertTrue(self.result()["errors"])

    def test_component_not_end_to_end(self):
        self.manifest["evidence"]["proof"]["level"] = "native_component"
        self.assertTrue(self.result()["errors"])

    def test_source_stale(self):
        (self.root / "go.mod").write_text("changed")
        self.assertTrue(self.result()["errors"])

    def test_source_pending(self):
        self.manifest["release_decision"] = "not_ready"
        self.manifest["source_verification"]["status"] = "partial"
        self.assertFalse(self.result()["ready"])

    def test_source_missing_hash(self):
        del self.manifest["source_verification"]["tree_sha256"]
        self.assertTrue(self.result()["errors"])

    def test_path_escape(self):
        self.manifest["evidence"]["proof"]["path"] = "../outside.txt"
        self.assertTrue(self.result()["errors"])

    def test_wrong_declared_ready(self):
        self.manifest["gates"][0].update(status="blocked", remaining=["missing runtime"])
        self.assertTrue(self.result()["errors"])

    def test_source_bad_status(self):
        self.manifest["source_verification"]["status"] = "unknown"
        self.assertTrue(self.result()["errors"])

    def test_invalid_top_level(self):
        self.assertTrue(readiness.inspect(self.root, [])["errors"])

    def test_source_matches_delivery_exclusions(self):
        (self.root / "internal").mkdir()
        (self.root / "internal/included.go").write_text("package included")
        before = readiness.source_digest(self.root)
        excluded = [
            "cmd/_native_probe/main.go",
            "deploy/secrets/key.txt",
            "deploy/generated/compose.yaml",
            "deploy/.env",
            "web/node_modules/example/index.js",
            "web/dist/index.js",
            "web/test-results/result.json",
            "converters/__pycache__/file.pyc",
            "converters/models/model.bin",
            "converters/.venv-test/lib/module.py",
            "tests/data/db.json",
            "tests/dumps/runtime.log",
            "deploy/cert.pem",
            "deploy/cert.key",
            "deploy/cert.crt",
            "tests/runtime.db",
            "tests/archive.zip",
            "examples/local-demo/credentials.json",
            "examples/local-demo/service-credentials.json",
            "examples/local-demo/demo-state.json",
            "examples/local-demo/seed-journal.json",
            "examples/local-demo/preflight-journal.json",
            "examples/local-demo/readiness-result.json",
            "examples/local-demo/.deployment-state.json",
            "examples/local-demo/test.secret",
        ]
        for name in excluded:
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("excluded diagnostic/runtime fixture")
        self.assertEqual(before, readiness.source_digest(self.root))

    def test_source_includes_contract_changes(self):
        before = readiness.source_digest(self.root)
        (self.root / "docs").mkdir()
        (self.root / "docs/mcp-tools.json").write_text("{}")
        self.assertNotEqual(before, readiness.source_digest(self.root))

    def test_source_includes_example_launchers(self):
        before = readiness.source_digest(self.root)
        example = self.root / "examples/local-demo/demo.py"
        example.parent.mkdir(parents=True)
        example.write_text("print('example')")
        self.assertNotEqual(before, readiness.source_digest(self.root))

    def test_source_skips_undelivered_symlinks(self):
        (self.root / "internal").mkdir()
        before = readiness.source_digest(self.root)
        (self.root / "internal/linked.txt").symlink_to(self.root / "proof.txt")
        self.assertEqual(before, readiness.source_digest(self.root))


if __name__ == "__main__":
    unittest.main(verbosity=2)
