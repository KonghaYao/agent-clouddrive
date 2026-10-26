"""Offline launcher checks. Mocks test orchestration only, never native acceptance."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("local_demo_launcher", Path(__file__).with_name("demo.py"))
demo = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(demo)
VERIFY_GATEWAY = demo.verify_gateway


def state():
    return {"schema": 1, "kind": demo.KIND, "project": "clouddrive-demo-123456abcdef", "repo": str(demo.REPO),
            "public_url": demo.ORIGIN, "storage_mode": "local", "phase": "new", "native_verified": False}


def config(runtime):
    app_env = {"DRIVE_PUBLIC_URL": demo.ORIGIN, "DRIVE_MCP_PUBLIC_URL": demo.ORIGIN + "/mcp", "DRIVE_ALLOWED_ORIGINS": demo.ORIGIN,
               "DRIVE_OIDC_ISSUER": demo.ORIGIN + "/identity/realms/clouddrive", "DRIVE_OCIS_INTERNAL_URL": "http://ocis:9200", "DRIVE_STORAGE_MODE": "local"}
    services = {name: {"environment": {}, "depends_on": {"init-volumes": {"condition": "service_completed_successfully"}}} for name in (*demo.CORE, *demo.OMITTED, "init-volumes")}
    for name in ("api", "worker", "maintenance"):
        services[name]["environment"] = dict(app_env)
    services["s3-init"]["image"] = "python:3.12.12-slim-bookworm@sha256:" + "0" * 64
    services["gateway"].update(ports=[{"host_ip": "127.0.0.1", "published": "8080", "target": 8080}], networks={"backend": {"aliases": ["drive.localhost"]}, "edge": None})
    services["keycloak"]["environment"]["KC_HOSTNAME"] = demo.ORIGIN + "/identity"
    services["ocis"]["environment"] = {"OCIS_OIDC_ISSUER": app_env["DRIVE_OIDC_ISSUER"], "OCIS_INSECURE": "false", "STORAGE_USERS_DRIVER": "ocis"}
    services["ocis"]["depends_on"]["s3-init"] = {"condition": "service_completed_successfully"}
    services["preview"]["depends_on"]["kkfileview"] = {"condition": "service_healthy"}
    services["preview"]["environment"]["KKFILEVIEW_URL"] = "http://kkfileview:8012"
    return {"services": services, "volumes": {"postgres": {"name": state()["project"] + "_postgres"}},
            "networks": {"backend": {"name": state()["project"] + "_backend", "internal": True}, "edge": {"name": state()["project"] + "_edge"}},
            "secrets": {"example": {"file": str(runtime / "secrets/example")}}}


def healthy():
    return [{"Service": name, "State": "running", "Health": "healthy"} for name in demo.CORE]


class LauncherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.state = state()
        demo.DOCKER_ENDPOINT = None
        self.gateway_patch = patch.object(demo, "verify_gateway")
        self.gateway_patch.start()

    def tearDown(self):
        self.gateway_patch.stop()
        self.temp.cleanup()
        demo.DOCKER_ENDPOINT = None

    def test_atomic_output_modes_ignore_restrictive_umask(self):
        old = os.umask(0o077)
        try:
            demo.atomic_write(self.root / "public-container-config", "ok", 0o444)
            demo.atomic_write(self.root / "private", "secret")
        finally:
            os.umask(old)
        self.assertEqual(stat.S_IMODE((self.root / "public-container-config").stat().st_mode), 0o444)
        self.assertEqual(stat.S_IMODE((self.root / "private").stat().st_mode), 0o600)

    def test_runtime_refuses_symlinks_hardlinks_foreign_and_world_writable(self):
        destination = self.root / "destination"
        destination.write_text("not touched")
        link = self.root / "linked"
        link.symlink_to(destination)
        with self.assertRaises(demo.DemoError): demo.no_symlinks(link)
        with self.assertRaises(demo.DemoError): demo.atomic_write(link, "overwrite")
        with self.assertRaises(demo.DemoError): demo.audit_runtime(self.root)
        link.unlink()
        os.link(destination, link)
        with self.assertRaises(demo.DemoError): demo.audit_runtime(self.root)
        link.unlink()
        self.root.chmod(0o755)
        with self.assertRaises(demo.DemoError): demo.audit_runtime(self.root)
        self.root.chmod(0o700)
        self.assertEqual(destination.read_text(), "not touched")

    def test_config_generation_is_outside_source_and_keeps_native_safety(self):
        original = (demo.DEPLOY / "compose.yaml").read_bytes()
        demo.prepare(self.root, self.state)
        self.assertEqual((demo.DEPLOY / "compose.yaml").read_bytes(), original)
        accounts = json.loads((self.root / "credentials.json").read_text())
        self.assertNotEqual(accounts["owner"]["password"], accounts["reader"]["password"])
        for role, account in accounts.items():
            self.assertRegex(account["username"], role + "-[0-9a-f]{16}$")
            self.assertGreaterEqual(len(account["password"]), 40)
        realm = json.loads((self.root / "generated/clouddrive-realm.json").read_text())
        human = [user for user in realm["users"] if "credentials" in user]
        self.assertEqual(len(human), 2)
        self.assertTrue(all(user["firstName"] and user["lastName"] for user in human))
        self.assertIn("ocis init >/dev/null 2>&1", (self.root / "ocis-entrypoint.sh").read_text())
        self.assertIn("DRIVE_OCIS_INTEGRATION_VERIFIED=false", (self.root / ".env").read_text())
        self.assertIn('"' + str(demo.REPO) + '"', (self.root / "compose.source.yaml").read_text())
        demo.load_state(self.root)
        with (self.root / ".env").open("a") as file: file.write("DRIVE_BIND_ADDRESS=0.0.0.0\n")
        with self.assertRaisesRegex(demo.DemoError, "changed"): demo.load_state(self.root)

    def test_unmarked_or_mismatched_runtime_is_not_adopted(self):
        with self.assertRaises(demo.DemoError): demo.load_state(self.root)
        demo.write_state(self.root, {**self.state, "public_url": "http://other.localhost:8080"})
        with self.assertRaises(demo.DemoError): demo.load_state(self.root)

    def test_reduced_compose_retains_real_services_and_no_optional_dependencies(self):
        reduced = demo.reduce_config(config(self.root), self.root, self.state)
        services = reduced["services"]
        self.assertFalse(set(demo.OMITTED) & set(services))
        self.assertTrue(set(demo.CORE) <= set(services))
        self.assertNotIn("KKFILEVIEW_URL", services["preview"]["environment"])
        for service in services.values():
            self.assertFalse(set(demo.OMITTED) & set(service.get("depends_on", {})))
        tools = services["demo-tools"]
        self.assertEqual(tools["entrypoint"], ["python", "/source/examples/local-demo/seed.py"])
        self.assertEqual(tools["volumes"][0], {"type": "bind", "source": str(demo.REPO), "target": "/source", "read_only": True})
        # At that mounted path parents[2] resolves to /source and finds harness.
        support = Path("/source/examples/local-demo/demo_support.py")
        self.assertEqual(str(support.parents[2] / "tests/integration"), "/source/tests/integration")
        self.assertTrue((demo.REPO / "tests/integration/deployed_api_harness.py").is_file())
        self.assertTrue(tools["read_only"])
        self.assertEqual(tools["cap_drop"], ["ALL"])
        self.assertIn("no-new-privileges:true", tools["security_opt"])
        self.assertEqual(tools["environment"]["PYTHONDONTWRITEBYTECODE"], "1")
        self.assertEqual(services["init-volumes"]["profiles"], ["tools"])

    def test_unsafe_resolved_config_is_rejected(self):
        for mutation in (
            lambda c: c["services"]["gateway"]["ports"][0].update(host_ip="0.0.0.0"),
            lambda c: c["services"]["gateway"]["ports"][0].update(published="8181"),
            lambda c: c["services"]["api"]["environment"].update(DRIVE_PUBLIC_URL="http://other.localhost:8080"),
            lambda c: c["services"]["ocis"]["environment"].update(OCIS_INSECURE="true"),
            lambda c: c["networks"]["backend"].update(internal=False),
            lambda c: c["volumes"]["postgres"].update(name="production_postgres"),
            lambda c: c["services"]["api"].update(privileged=True),
        ):
            candidate = config(self.root); mutation(candidate)
            with self.subTest(mutation=mutation), self.assertRaises(demo.DemoError): demo.reduce_config(candidate, self.root, self.state)

    def test_host_env_cannot_override_bind_origin_storage_or_compose(self):
        polluted = {"DRIVE_BIND_ADDRESS": "0.0.0.0", "DRIVE_PUBLIC_URL": "http://evil.test", "STORAGE_USERS_DRIVER": "s3ng", "COMPOSE_FILE": "bad.yml"}
        with patch.dict(os.environ, polluted), patch.object(demo.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "ok", "")) as run:
            self.assertEqual(demo.command(["docker", "compose", "config"], "test"), "ok")
            self.assertFalse(set(polluted) & set(run.call_args.kwargs["env"]))

    def test_remote_docker_context_precedence_and_pinning(self):
        with patch.object(demo.shutil, "which", return_value="/bin/docker"), patch.dict(os.environ, {"DOCKER_HOST": "unix:///local.sock", "DOCKER_CONTEXT": "remote"}), patch.object(demo, "command", side_effect=["v2", '"ssh://remote"']) as command:
            with self.assertRaisesRegex(demo.DemoError, "remote daemon"): demo.docker_ready()
            self.assertFalse(any("info" in call.args[0] for call in command.call_args_list))
        demo.DOCKER_ENDPOINT = "unix:///safe.sock"
        with patch.dict(os.environ, {"DOCKER_HOST": "tcp://remote:2375", "DOCKER_CONTEXT": "remote"}), patch.object(demo.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "", "")) as run:
            demo.command(["docker", "compose", "up"], "test")
            self.assertEqual(run.call_args.args[0][:3], ["docker", "--host", "unix:///safe.sock"])
            self.assertNotIn("DOCKER_CONTEXT", run.call_args.kwargs["env"])
            self.assertNotIn("DOCKER_HOST", run.call_args.kwargs["env"])

    def test_child_failure_never_prints_secret_and_timeout_is_bounded(self):
        with patch.object(demo.subprocess, "run", return_value=subprocess.CompletedProcess([], 19, "secret-token", "secret-password")):
            with self.assertRaises(demo.DemoError) as result: demo.command(["tool"], "safe stage")
            self.assertIn("exit 19", str(result.exception)); self.assertNotIn("secret", str(result.exception).replace("secrets", ""))
        with patch.object(demo.subprocess, "run", side_effect=subprocess.TimeoutExpired(["tool"], 1)):
            with self.assertRaisesRegex(demo.DemoError, "timed out"): demo.command(["tool"], "safe", timeout=1)

    def test_port_conflict_never_silently_changes_origin(self):
        fake = unittest.mock.MagicMock()
        fake.__enter__.return_value.bind.side_effect = OSError("busy")
        with patch.object(demo.socket, "socket", return_value=fake):
            with self.assertRaisesRegex(demo.DemoError, "never silently changes"): demo.port_available([])
            demo.port_available([{"Service": "gateway", "State": "running"}])

    def test_status_formats_and_unhealthy_fail(self):
        self.assertEqual(demo.rows_from_json('{"Service":"api"}\n{"Service":"worker"}'), [{"Service": "api"}, {"Service": "worker"}])
        demo.check_services(healthy())
        rows = healthy(); rows[0]["Health"] = "unhealthy"
        with self.assertRaisesRegex(demo.DemoError, "postgres"): demo.check_services(rows)

    def test_published_gateway_checks_built_js_and_exact_oidc_origin(self):
        issuer = demo.ORIGIN + "/identity/realms/clouddrive"
        bodies = [json.dumps({"oidc_issuer": issuer, "oidc_client_id": "clouddrive-web"}).encode(),
                  json.dumps({"issuer": issuer, "token_endpoint": issuer + "/protocol/openid-connect/token"}).encode(),
                  b'<div id="root"></div><script type="module" src="/assets/main-123.js"></script>', b'/* built JS */']
        def connection(*args, **kwargs):
            conn = unittest.mock.MagicMock()
            conn.getresponse.return_value.status = 200
            conn.getresponse.return_value.read.return_value = bodies.pop(0)
            return conn
        with patch.object(demo.http.client, "HTTPConnection", side_effect=connection) as calls:
            VERIFY_GATEWAY()
            self.assertEqual(calls.call_count, 4)
            self.assertTrue(all(c.args == ("127.0.0.1", 8080) for c in calls.call_args_list))
        with patch.object(demo.http.client, "HTTPConnection", side_effect=OSError("unavailable")):
            with self.assertRaises(OSError): VERIFY_GATEWAY()

    def test_preflight_requires_complete_project_bound_real_proofs(self):
        proofs = {key: True for key in ("pkce", "confidential_exchange_openid", "personal_space", "tus_create", "tus_overwrite", "revision_read", "move_stable_id", "move_retained_history", "stale_admission_denied")}
        result = {"schema": 1, "kind": demo.KIND + "-native-preflight", "project": self.state["project"], "status": "passed", "proofs": proofs, "limitations": {"native_final_commit_cas": False, "current_get_if_match_enforcement": False, "business_durable_operation_gates_required": True}}
        demo.save_json(self.root / "preflight-result.json", result)
        demo.verify_preflight(self.root, self.state)
        for invalid in ({**result, "project": "other"}, {**result, "proofs": {**proofs, "revision_read": False}}, {**result, "status": "running"}):
            demo.save_json(self.root / "preflight-result.json", invalid)
            with self.assertRaises(demo.DemoError): demo.verify_preflight(self.root, self.state)

    def test_orchestration_waits_then_migrates_and_never_gates_failed_probe(self):
        (self.root / "compose.json").write_text("{}")
        calls = []
        def compose(runtime, state, *args, **kwargs):
            calls.append(("compose", args)); return ""
        def tools(runtime, state, *args, **kwargs):
            calls.append(("tools", args))
        def gate(runtime, state, enabled): calls.append(("gate", enabled))
        with patch.object(demo, "compose", side_effect=compose), patch.object(demo, "service_rows", side_effect=[[], healthy()]), patch.object(demo, "port_available"), patch.object(demo, "gates", side_effect=gate), patch.object(demo, "tools_run", side_effect=tools), patch.object(demo, "verify_preflight"), patch.object(demo, "verify_seed"):
            demo.start(self.root, self.state)
        wait = next(i for i, c in enumerate(calls) if c[0] == "compose" and "--wait" in c[1] and "postgres" in c[1])
        migrate = next(i for i, c in enumerate(calls) if c[0] == "compose" and "migrate" in c[1])
        preflight = calls.index(("tools", ("--preflight",)))
        enabled = calls.index(("gate", True))
        seeded = calls.index(("tools", ("--allow-synthetic-offline-consent",)))
        self.assertLess(wait, migrate); self.assertLess(migrate, preflight); self.assertLess(preflight, enabled); self.assertLess(enabled, seeded)
        self.assertEqual(self.state["phase"], "ready")
        self.state.pop("verified_at"); self.state["phase"] = "prepared"
        with patch.object(demo, "compose", return_value=""), patch.object(demo, "service_rows", return_value=[]), patch.object(demo, "port_available"), patch.object(demo, "gates") as gates, patch.object(demo, "tools_run", side_effect=demo.DemoError("native probe failed")):
            with self.assertRaises(demo.DemoError): demo.start(self.root, self.state)
            gates.assert_called_once_with(self.root, self.state, False)
        self.assertNotEqual(self.state["phase"], "ready")

    def test_repeated_start_does_not_reseed_or_overwrite_user_edits(self):
        (self.root / "compose.json").write_text("{}")
        self.state.update(phase="ready", verified_at="previous")
        with patch.object(demo, "service_rows", return_value=healthy()), patch.object(demo, "port_available"), patch.object(demo, "tools_run") as tools, patch.object(demo, "compose") as compose:
            demo.start(self.root, self.state)
            tools.assert_called_once_with(self.root, self.state, "--ready")
            compose.assert_not_called()

    def test_stop_only_stops_keeps_data_and_never_downs_volumes(self):
        demo.write_state(self.root, self.state)
        (self.root / "compose.json").write_text("{}")
        with patch.object(demo, "docker_ready"), patch.object(demo, "compose") as compose:
            self.state["phase"] = "prepared"; demo.write_state(self.root, self.state)
            self.assertEqual(demo.main(["stop", "--runtime", str(self.root)]), 0)
            self.assertEqual(compose.call_count, 1)
            self.assertEqual(compose.call_args.args[0], self.root)
            self.assertEqual(compose.call_args.args[2], "stop")
            self.assertEqual(compose.call_args.kwargs, {"timeout": 120})
        self.assertTrue((self.root / "compose.json").exists())
        self.assertEqual(json.loads((self.root / "demo-state.json").read_text())["phase"], "stopped")
        source = Path(demo.__file__).read_text()
        self.assertNotIn('"down"', source)
        self.assertNotIn('"--volumes"', source)


if __name__ == "__main__":
    unittest.main(verbosity=2)
