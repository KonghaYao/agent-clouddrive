import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))


class DeploymentTests(unittest.TestCase):
    def run_setup(self, path, *args):
        return subprocess.run([sys.executable, str(HERE.parent / "setup.py"), "--directory", str(path), *args], capture_output=True, text=True)

    def test_local_and_s3_generation_idempotent(self):
        for mode, driver in (("local", "ocis"), ("s3", "s3ng")):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                path = Path(directory)
                result = self.run_setup(path, "--development-http", "--mode", mode)
                self.assertEqual(result.returncode, 0, result.stderr)
                before = {p.name: p.read_bytes() for p in (path / "secrets").iterdir()}
                result = self.run_setup(path, "--development-http", "--mode", mode)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(before, {p.name: p.read_bytes() for p in (path / "secrets").iterdir()})
                self.assertEqual(len(before["delegation_key"]), 32)
                self.assertIn("STORAGE_USERS_DRIVER=" + driver, (path / ".env").read_text())
                self.assertIn("DRIVE_STORAGE_MODE=" + mode, (path / ".env").read_text())
                self.assertIn("DRIVE_DELEGATION_VERIFIED=false", (path / ".env").read_text())
                self.assertIn("DRIVE_OCIS_INTEGRATION_VERIFIED=false", (path / ".env").read_text())
                self.assertIn("DRIVE_VISION_PROVIDER_ID=unconfigured", (path / ".env").read_text())
                self.assertEqual((path / "secrets").stat().st_mode & 0o777, 0o700)
                self.assertEqual(json.loads((path / "generated/config.json").read_text())["oidc_audience"], "agent-clouddrive")
                compose = os.environ.get("COMPOSE_BIN", "/tmp/docker-compose")
                if Path(compose).is_file():
                    shutil.copy(HERE.parent / "compose.yaml", path / "compose.yaml")
                    check = subprocess.run([compose, "--project-directory", str(path), "config", "--quiet"], capture_output=True, text=True)
                    self.assertEqual(check.returncode, 0, check.stderr)

    def test_http_cannot_silently_be_production(self):
        with tempfile.TemporaryDirectory() as path:
            self.assertNotEqual(self.run_setup(path).returncode, 0)
            self.assertNotEqual(self.run_setup(path, "--development-http", "--public-url", "http://example.com:8080").returncode, 0)
            self.assertNotEqual(self.run_setup(path, "--development-http", "--public-url", "http://drive.localhost:8888").returncode, 0)

    def test_no_silent_storage_migration(self):
        with tempfile.TemporaryDirectory() as path:
            self.assertEqual(self.run_setup(path, "--development-http", "--mode", "local").returncode, 0)
            result = self.run_setup(path, "--development-http", "--mode", "s3")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not a migration", result.stderr)

    def test_reference_realm_security(self):
        from keycloak.realm_template import realm
        value = realm("https://drive.example.test", "backend-test", "identity-test")
        clients = {c["clientId"]: c for c in value["clients"]}
        for name in ("clouddrive-web", "peri", "clouddrive-background"):
            self.assertEqual(clients[name]["attributes"]["pkce.code.challenge.method"], "S256")
            self.assertFalse(clients[name]["directAccessGrantsEnabled"])
        self.assertEqual(value["sslRequired"], "external")
        self.assertEqual(clients["peri"]["redirectUris"], [])
        self.assertTrue(all("credentials" not in u for u in value["users"]))
        self.assertIn("offline_access", clients["clouddrive-background"]["optionalClientScopes"])
        self.assertFalse(clients["agent-clouddrive"]["consentRequired"])
        self.assertFalse(clients["agent-clouddrive"]["standardFlowEnabled"])
        self.assertNotIn("offline_access", clients["clouddrive-web"]["optionalClientScopes"])
        scopes = {scope["name"]: scope for scope in value["clientScopes"]}
        self.assertTrue(all("basic" in client["defaultClientScopes"] for client in clients.values()))
        self.assertTrue(any(mapper["protocolMapper"] == "oidc-sub-mapper" and mapper["config"].get("access.token.claim") == "true" for mapper in scopes["basic"]["protocolMappers"]))


if __name__ == "__main__":
    unittest.main(verbosity=2)
