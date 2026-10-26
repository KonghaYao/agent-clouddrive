#!/usr/bin/env python3
"""Start an isolated, persistent, loopback-only CloudDrive example (Python 3.10+)."""
from __future__ import annotations

import argparse
import contextlib
import datetime
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import stat
import subprocess
import sys

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
DEPLOY = REPO / "deploy" / "compose"
ORIGIN = "http://drive.localhost:8080"
KIND = "agent-clouddrive-local-demo"
DOCKER_ENDPOINT = None
CORE = ("postgres", "nats", "meilisearch", "keycloak", "api", "gateway", "ocis", "anydoc", "preview", "worker", "maintenance")
OMITTED = ("rustfs", "s3-init", "paddleocr", "prepare-models", "kkfileview")
GATES = ("DRIVE_OCIS_INTEGRATION_VERIFIED", "DRIVE_DELEGATION_VERIFIED")


def deploy_files():
    """Yield deployable source files by relative path, including nested services."""
    for path in DEPLOY.rglob("*"):
        relative = path.relative_to(DEPLOY)
        if (not path.is_file() or relative.name in ("compose.yaml", ".env")
                or relative.parts[0] in ("generated", "secrets")
                or any(part.startswith(".") or part == "__pycache__" for part in relative.parts)):
            continue
        yield relative


class DemoError(Exception):
    pass


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def default_runtime():
    identifier = hashlib.sha256(str(REPO).encode()).hexdigest()[:12]
    return Path.home() / ".local" / "share" / "agent-clouddrive-demo" / identifier


def no_symlinks(path):
    """Check before resolving: a symlink must never silently choose our target."""
    path = Path(os.path.abspath(os.path.expanduser(str(path))))
    for part in (path, *path.parents):
        if part.is_symlink():
            raise DemoError("Refusing a runtime path with a symbolic-link component.")
    return path


def audit_runtime(path):
    no_symlinks(path)
    if not path.is_dir() or path.stat().st_uid != os.getuid() or stat.S_IMODE(path.stat().st_mode) != 0o700:
        raise DemoError("Runtime must be your own directory with permissions 0700.")
    for entry in path.rglob("*"):
        info = entry.lstat()
        if stat.S_ISLNK(info.st_mode) or not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)):
            raise DemoError("Refusing symbolic links or special files inside the runtime.")
        if info.st_uid != os.getuid() or info.st_mode & 0o022 or (entry.is_file() and info.st_nlink != 1):
            raise DemoError("Runtime contains foreign-owned, externally writable, or hard-linked files.")


def atomic_write(path, data, mode=0o600):
    if path.is_symlink() or (path.exists() and (not path.is_file() or path.stat().st_nlink != 1)):
        raise DemoError("Refusing an unsafe runtime output file.")
    temporary = path.with_name(path.name + ".tmp-" + secrets.token_hex(6))
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w") as file:
            file.write(data)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def save_json(path, value, mode=0o600):
    atomic_write(path, json.dumps(value, indent=2) + "\n", mode)


def load_state(runtime):
    audit_runtime(runtime)
    try:
        value = json.loads((runtime / "demo-state.json").read_text())
    except (OSError, ValueError):
        raise DemoError("Runtime has no valid demo-state.json; refusing to adopt existing data.") from None
    if (not isinstance(value, dict) or value.get("schema") != 1 or value.get("kind") != KIND or value.get("public_url") != ORIGIN
            or value.get("storage_mode") != "local" or value.get("repo") != str(REPO)
            or not re.fullmatch(r"clouddrive-demo-[0-9a-f]{12}", value.get("project", ""))):
        raise DemoError("Runtime does not belong to this example and source directory.")
    if not isinstance(value.get("managed_hashes", {}), dict):
        raise DemoError("Runtime integrity manifest is invalid.")
    for name, digest in value.get("managed_hashes", {}).items():
        relative = Path(name)
        if relative.is_absolute() or ".." in relative.parts:
            raise DemoError("Runtime integrity manifest is invalid.")
        path = runtime / relative
        if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise DemoError("Managed runtime configuration changed; refusing to launch altered settings.")
    return value


def write_state(runtime, state):
    save_json(runtime / "demo-state.json", state)


def record_hashes(runtime, state):
    names = [".env", "compose.source.yaml", "compose.json", "credentials.json", "service-credentials.json"]
    names += [str(p) for p in deploy_files()]
    names += [str(p.relative_to(runtime)) for folder in ("generated", "secrets") for p in (runtime / folder).glob("*") if p.is_file()]
    state["managed_hashes"] = {name: hashlib.sha256((runtime / name).read_bytes()).hexdigest() for name in names if (runtime / name).is_file()}
    write_state(runtime, state)


@contextlib.contextmanager
def runtime_lock(runtime):
    fd = os.open(runtime / ".launcher.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DemoError("Another launcher is operating this runtime. Wait for it to finish.") from None
        yield
    finally:
        os.close(fd)


def command(args, stage, timeout=600, capture=True):
    """Never echo child output: it can contain credentials or authorization URLs."""
    env = {key: val for key, val in os.environ.items() if not key.startswith(("COMPOSE_", "DRIVE_", "STORAGE_USERS_"))}
    env["COMPOSE_ANSI"] = "never"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    if args[0] == "docker" and DOCKER_ENDPOINT:
        args = ["docker", "--host", DOCKER_ENDPOINT, *args[1:]]
        for key in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"):
            env.pop(key, None)
    try:
        result = subprocess.run(args, cwd=REPO, env=env, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout)
    except FileNotFoundError:
        raise DemoError(stage + ": required executable is not installed.") from None
    except subprocess.TimeoutExpired:
        raise DemoError(stage + ": timed out; retained containers/data were not removed.") from None
    if result.returncode:
        raise DemoError(stage + f": failed (exit {result.returncode}). No child output is printed because it may contain secrets.")
    return result.stdout if capture else None


def docker_ready():
    global DOCKER_ENDPOINT
    DOCKER_ENDPOINT = None
    if not shutil.which("docker"):
        raise DemoError("Docker Engine/Desktop and Docker Compose v2 are required. No containers were started.")
    command(["docker", "compose", "version"], "Docker Compose v2 check", 30)
    endpoint = None if os.environ.get("DOCKER_CONTEXT") else os.environ.get("DOCKER_HOST")
    if not endpoint:
        raw = command(["docker", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}"], "Docker context check", 30)
        try:
            endpoint = json.loads(raw)
        except ValueError:
            raise DemoError("Cannot verify Docker context endpoint.") from None
    if not isinstance(endpoint, str) or not endpoint.startswith(("unix://", "npipe://")):
        raise DemoError("Use a local Docker Desktop/Engine context, not a remote daemon: the browser URL must be local.")
    DOCKER_ENDPOINT = endpoint
    command(["docker", "info", "--format", "{{json .ServerVersion}}"], "Docker daemon check; start Docker and retry", 30)


def compose(runtime, state, *args, stage=None, timeout=600, source=False):
    filename = "compose.source.yaml" if source else "compose.json"
    return command(["docker", "compose", "--project-directory", str(runtime), "--env-file", str(runtime / ".env"),
                    "--project-name", state["project"], "--file", str(runtime / filename), *args],
                   stage or ("Compose " + args[0]), timeout)


def rows_from_json(raw):
    if not raw.strip():
        return []
    try:
        rows = json.loads(raw)
        return rows if isinstance(rows, list) else [rows]
    except ValueError:
        try:
            return [json.loads(line) for line in raw.splitlines() if line.strip()]
        except ValueError:
            raise DemoError("Docker returned an unreadable service status.") from None


def service_rows(runtime, state):
    return rows_from_json(compose(runtime, state, "ps", "--all", "--format", "json", timeout=30))


def port_available(rows):
    own_gateway = any(row.get("Service") == "gateway" and row.get("State") == "running" for row in rows)
    if own_gateway:
        return
    with socket.socket() as sock:
        try:
            sock.bind(("127.0.0.1", 8080))
        except OSError:
            raise DemoError("127.0.0.1:8080 is already in use by another process/project. Stop that listener and retry; this demo never silently changes its OIDC origin or port.") from None


def render_source(project):
    source = (DEPLOY / "compose.yaml").read_text()
    source = source.replace("name: agent-clouddrive", "name: " + project, 1)
    source = source.replace("context: ../..", "context: " + json.dumps(str(REPO)))
    return source.replace("agent-clouddrive/", project + "/")


def copy_deploy_files(runtime):
    for relative in deploy_files():
        source = DEPLOY / relative
        content = source.read_text()
        if source.name == "ocis-entrypoint.sh":
            # oCIS initialization can print generated credentials. Preserve its
            # exit status, but never send that output to Docker logs.
            content = content.replace("then ocis init; fi", "then ocis init >/dev/null 2>&1; fi")
        destination = runtime / relative
        destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        destination.parent.chmod(0o700)
        atomic_write(destination, content, 0o444)


def prepare(runtime, state):
    command([sys.executable, str(DEPLOY / "setup.py"), "--mode", "local", "--development-http", "--public-url", ORIGIN,
             "--directory", str(runtime)], "Generate isolated local configuration", 30)
    credentials = {}
    for role in ("owner", "reader"):
        credentials[role] = {"username": role + "-" + secrets.token_hex(8), "password": secrets.token_urlsafe(32)}
    save_json(runtime / "credentials.json", credentials)
    save_json(runtime / "service-credentials.json", json.loads((runtime / "secrets/identity_service_credential.json").read_text()))
    realm_path = runtime / "generated/clouddrive-realm.json"
    realm = json.loads(realm_path.read_text())
    for role, account in credentials.items():
        realm["users"].append({"username": account["username"], "enabled": True, "email": account["username"] + "@synthetic.invalid",
                               "emailVerified": True, "firstName": "Synthetic", "lastName": role, "realmRoles": ["offline_access"],
                               "credentials": [{"type": "password", "value": account["password"], "temporary": False}]})
    save_json(realm_path, realm, 0o444)
    copy_deploy_files(runtime)
    atomic_write(runtime / "compose.source.yaml", render_source(state["project"]))
    state["phase"] = "configured"
    record_hashes(runtime, state)


def reduce_config(config, runtime, state):
    """Keep official services/security/images; remove only disclosed optional paths."""
    config["name"] = state["project"]
    services = config["services"]
    python_image = services["s3-init"]["image"]
    for name in OMITTED:
        services.pop(name, None)
    for service in services.values():
        for name in (*OMITTED, "init-volumes"):
            service.get("depends_on", {}).pop(name, None)
        if not service.get("depends_on"):
            service.pop("depends_on", None)
    services["init-volumes"]["profiles"] = ["tools"]
    services["preview"]["environment"].pop("KKFILEVIEW_URL", None)
    services["anydoc"]["environment"]["DRIVE_OCR_URL"] = ""
    for name in ("api", "worker", "maintenance"):
        for gate in GATES:
            services[name]["environment"][gate] = "false"
    services["demo-tools"] = {
        "image": python_image, "profiles": ["tools"], "restart": "no", "init": True,
        "entrypoint": ["python", "/source/examples/local-demo/seed.py"], "working_dir": "/source/examples/local-demo",
        "user": f"{os.getuid()}:{os.getgid()}", "networks": {"backend": None},
        "environment": {"PYTHONDONTWRITEBYTECODE": "1", "PYTHONUNBUFFERED": "1"},
        "volumes": [{"type": "bind", "source": str(REPO), "target": "/source", "read_only": True},
                    {"type": "bind", "source": str(runtime), "target": "/runtime"}],
        "read_only": True, "tmpfs": ["/tmp:size=64m,mode=1777"], "mem_limit": "512m", "pids_limit": 128,
        "security_opt": ["no-new-privileges:true"], "cap_drop": ["ALL"]}
    validate_config(config, runtime, state)
    return config


def validate_config(config, runtime, state):
    services = config["services"]
    expected = {"DRIVE_PUBLIC_URL": ORIGIN, "DRIVE_MCP_PUBLIC_URL": ORIGIN + "/mcp", "DRIVE_ALLOWED_ORIGINS": ORIGIN,
                "DRIVE_OIDC_ISSUER": ORIGIN + "/identity/realms/clouddrive", "DRIVE_OCIS_INTERNAL_URL": "http://ocis:9200", "DRIVE_STORAGE_MODE": "local"}
    ports = services["gateway"].get("ports", [])
    if (len(ports) != 1 or ports[0].get("host_ip") != "127.0.0.1" or str(ports[0].get("published")) != "8080"
            or str(ports[0].get("target")) != "8080" or services["gateway"]["networks"]["backend"].get("aliases") != ["drive.localhost"]
            or config["networks"]["backend"].get("internal") is not True):
        raise DemoError("Generated gateway/network configuration violates the fixed loopback origin.")
    for name, service in services.items():
        if name != "gateway" and service.get("ports"):
            raise DemoError("A private dependency unexpectedly publishes a host port.")
        if service.get("privileged") or service.get("network_mode") == "host" or service.get("pid") == "host" or service.get("devices"):
            raise DemoError("Unsafe Docker service settings are not permitted.")
    for name in ("api", "worker", "maintenance"):
        if any(services[name]["environment"].get(key) != value for key, value in expected.items()):
            raise DemoError("Application origin/storage configuration is inconsistent.")
    if (services["keycloak"]["environment"].get("KC_HOSTNAME") != ORIGIN + "/identity"
            or services["ocis"]["environment"].get("OCIS_OIDC_ISSUER") != expected["DRIVE_OIDC_ISSUER"]
            or services["ocis"]["environment"].get("OCIS_INSECURE") != "false"
            or services["ocis"]["environment"].get("STORAGE_USERS_DRIVER") != "ocis"):
        raise DemoError("Identity/native-storage origin or security configuration is inconsistent.")
    for kind in ("volumes", "networks"):
        if any(value.get("external") or not value.get("name", "").startswith(state["project"] + "_") for value in config.get(kind, {}).values()):
            raise DemoError("Docker resources must be private to this unique demo project.")
    for secret in config.get("secrets", {}).values():
        if "file" not in secret or not Path(secret["file"]).is_relative_to(runtime / "secrets"):
            raise DemoError("Docker secret references must stay within this private runtime.")


def create_compose(runtime, state):
    raw = compose(runtime, state, "config", "--format", "json", source=True, stage="Resolve pinned reference Compose configuration", timeout=60)
    try:
        config = reduce_config(json.loads(raw), runtime, state)
    except (ValueError, KeyError, TypeError):
        raise DemoError("Reference Compose schema changed; cannot safely produce local configuration.") from None
    save_json(runtime / "compose.json", config)
    compose(runtime, state, "config", "--quiet", stage="Validate local Compose configuration", timeout=60)
    state["phase"] = "prepared"
    record_hashes(runtime, state)


def gates(runtime, state, enabled):
    config = json.loads((runtime / "compose.json").read_text())
    for name in ("api", "worker", "maintenance"):
        for gate in GATES:
            config["services"][name]["environment"][gate] = str(enabled).lower()
    validate_config(config, runtime, state)
    save_json(runtime / "compose.json", config)
    state["native_verified"] = enabled
    record_hashes(runtime, state)


def verify_gateway():
    """Verify the actually published loopback listener, not just Docker healthz.

    Keep Host/OIDC origin unchanged; direct loopback connection avoids relying
    on the host OS resolver's optional .localhost support (browsers support it).
    """
    def fetch(path):
        connection = http.client.HTTPConnection("127.0.0.1", 8080, timeout=15)
        try:
            connection.request("GET", path, headers={"Host": "drive.localhost:8080"})
            response = connection.getresponse()
            body = response.read(2 * 1024 * 1024 + 1)
            if response.status != 200 or len(body) > 2 * 1024 * 1024:
                raise DemoError("The loopback gateway did not serve the expected frontend/identity resource.")
            return body
        except (OSError, http.client.HTTPException):
            raise DemoError("Cannot reach this demo at 127.0.0.1:8080 with the configured public Host.") from None
        finally:
            connection.close()
    try:
        configuration = json.loads(fetch("/config.json"))
        discovery = json.loads(fetch("/identity/realms/clouddrive/.well-known/openid-configuration"))
    except ValueError:
        raise DemoError("The published frontend/identity configuration is invalid.") from None
    issuer = ORIGIN + "/identity/realms/clouddrive"
    if (not isinstance(configuration, dict) or not isinstance(discovery, dict) or configuration.get("oidc_issuer") != issuer or configuration.get("oidc_client_id") != "clouddrive-web"
            or discovery.get("issuer") != issuer or discovery.get("token_endpoint") != issuer + "/protocol/openid-connect/token"):
        raise DemoError("The published frontend and identity issuer do not match the fixed public origin.")
    html = fetch("/")
    asset = re.search(rb'<script[^>]+src="(/assets/[A-Za-z0-9_./-]+\.js)"', html)
    if b'<div id="root"' not in html or not asset or b".." in asset.group(1):
        raise DemoError("The gateway has not served the built application frontend.")
    if not fetch(asset.group(1).decode("ascii")):
        raise DemoError("The built frontend JavaScript is empty.")


def verify_preflight(runtime, state):
    try:
        result = json.loads((runtime / "preflight-result.json").read_text())
    except (OSError, ValueError):
        raise DemoError("Native preflight did not produce a valid proof record; gates remain disabled.") from None
    required = ("pkce", "confidential_exchange_openid", "personal_space", "tus_create", "tus_overwrite", "revision_read",
                "move_stable_id", "move_retained_history", "stale_admission_denied")
    if (not isinstance(result, dict) or result.get("schema") != 1 or result.get("kind") != KIND + "-native-preflight"
            or result.get("project") != state["project"] or result.get("status") != "passed"
            or not isinstance(result.get("proofs"), dict)
            or any(result["proofs"].get(key) is not True for key in required)
            or result.get("limitations") != {"native_final_commit_cas": False, "current_get_if_match_enforcement": False,
                                             "business_durable_operation_gates_required": True}):
        raise DemoError("Native preflight proof is incomplete or belongs to another project; gates remain disabled.")


def verify_seed(runtime, state):
    try:
        result = json.loads((runtime / "demo-result.json").read_text())
    except (OSError, ValueError):
        raise DemoError("Sample workflow did not produce a valid completion record.") from None
    required = ("pkce_profile_and_web_session", "real_tus_upload", "immutable_two_versions",
                "current_source_search_ready", "controlled_pdf_png", "reader_private_isolation")
    if (not isinstance(result, dict) or result.get("schema") != 1 or result.get("status") != "passed"
            or result.get("project") != state["project"] or result.get("scope") != "real-disposable-demo-api"
            or not isinstance(result.get("checks"), dict) or any(result["checks"].get(key) != "passed" for key in required)):
        raise DemoError("Sample completion record is incomplete or belongs to another project.")


def check_services(rows):
    by_service = {row.get("Service"): row for row in rows}
    bad = [name for name in CORE if name not in by_service or by_service[name].get("State") != "running"
           or by_service[name].get("Health") not in (None, "", "healthy")]
    if bad:
        raise DemoError("Services are not ready: " + ", ".join(bad) + ". Run start to resume, or inspect this project's Docker state locally.")


def tools_run(runtime, state, *options, timeout=900):
    return compose(runtime, state, "run", "--rm", "--no-deps", "-T", "demo-tools", "--runtime", "/runtime", "--container-network", *options,
                   stage="Live " + ("native integration preflight" if "--preflight" in options else "sample workflow verification"), timeout=timeout)


def start(runtime, state, restart=False):
    if not (runtime / "compose.json").is_file():
        create_compose(runtime, state)
    rows = service_rows(runtime, state)
    port_available(rows)
    if restart:
        print("Stopping this demo's containers; all data and credentials are retained.", flush=True)
        compose(runtime, state, "stop", timeout=120)
    # An already-ready demo is never re-seeded or overwritten on repeated start.
    if state.get("phase") == "ready" and not restart:
        try:
            check_services(rows)
        except DemoError:
            pass
        else:
            verify_gateway()
            tools_run(runtime, state, "--ready")
            return
    print("Building the real frontend, API and converters. First start can take several minutes.", flush=True)
    compose(runtime, state, "build", "api", "gateway", "anydoc", "preview", stage="Build real application images", timeout=3600)
    gates(runtime, state, False)
    state["phase"] = "starting"
    write_state(runtime, state)
    compose(runtime, state, "run", "--rm", "--no-deps", "-T", "init-volumes", stage="Initialize isolated named-volume ownership", timeout=120)
    compose(runtime, state, "up", "--detach", "--wait", "--wait-timeout", "300", "postgres", "nats", "meilisearch", "keycloak", timeout=360)
    print("Dependencies are healthy; applying database migrations before starting the application.", flush=True)
    compose(runtime, state, "run", "--rm", "--no-deps", "-T", "maintenance", "/app/maintenance", "migrate", stage="Database migration", timeout=180)
    compose(runtime, state, "up", "--detach", "--wait", "--wait-timeout", "360", "api", "gateway", "ocis", "anydoc", "preview", timeout=420)
    verify_gateway()
    print("Checking real OIDC identity, delegated native storage, stale-write admission and retained history.", flush=True)
    tools_run(runtime, state, "--preflight")
    verify_preflight(runtime, state)
    gates(runtime, state, True)
    compose(runtime, state, "up", "--detach", "--wait", "--wait-timeout", "300", *CORE, timeout=360)
    # gates() rewrites api/worker environment, so the final up recreates those
    # containers with new addresses; make nginx re-resolve its upstreams.
    compose(runtime, state, "exec", "-T", "gateway", "nginx", "-s", "reload", stage="Refresh gateway upstreams", timeout=60)
    if state.get("verified_at"):
        print("Verifying live login/API readiness; preserving your existing sample edits.", flush=True)
        tools_run(runtime, state, "--ready")
    else:
        print("Running real sample uploads, conversion, preview and search checks.", flush=True)
        tools_run(runtime, state, "--allow-synthetic-offline-consent", timeout=1200)
        verify_seed(runtime, state)
    check_services(service_rows(runtime, state))
    state.update(phase="ready", last_ready_at=now())
    state.setdefault("verified_at", now())
    write_state(runtime, state)


def show(runtime, state):
    print("URL: " + ORIGIN)
    print("Private synthetic login credentials: " + str(runtime / "credentials.json"))
    print("Docker project: " + state["project"])
    print("Local-only example: text/PDF conversion and PDF/image preview. OCR, external vision, Office preview, RustFS/S3 and Peri-client acceptance are not included.")
    print("HTTP is intentional only on loopback. Do not expose this demo or use real sensitive files.")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("start", "check", "status", "stop", "restart"))
    parser.add_argument("--runtime", type=Path, default=default_runtime(), help="private state directory outside the repository")
    args = parser.parse_args(argv)
    os.umask(0o077)
    runtime = no_symlinks(args.runtime)
    if runtime == REPO or REPO in runtime.parents or runtime in REPO.parents:
        raise DemoError("Runtime must be outside, and not a parent of, the source repository.")
    docker_ready()
    if not runtime.exists():
        if args.action in ("check", "status", "stop"):
            if args.action == "check":
                port_available([])
            print("Docker is available; this demo has not been initialized. Run start. No live acceptance has run.")
            return 1 if args.action == "check" else 0
        runtime.mkdir(parents=True, mode=0o700)
        runtime.chmod(0o700)
        state = {"schema": 1, "kind": KIND, "project": "clouddrive-demo-" + secrets.token_hex(6), "repo": str(REPO),
                 "public_url": ORIGIN, "storage_mode": "local", "created_at": now(), "phase": "new", "native_verified": False}
        write_state(runtime, state)
    load_state(runtime)  # Do not create even a lock file in foreign runtime directories.
    with runtime_lock(runtime):
        state = load_state(runtime)
        if state["phase"] == "new":
            if args.action not in ("start", "restart"):
                print("Demo initialization is incomplete. Run start to continue.")
                return 1 if args.action == "check" else 0
            prepare(runtime, state)
        if not (runtime / "compose.json").exists():
            if args.action in ("status", "stop", "check"):
                print("Configuration prepared; no live acceptance has run. Run start to continue.")
                return 1 if args.action == "check" else 0
            create_compose(runtime, state)
        if args.action in ("start", "restart"):
            start(runtime, state, restart=args.action == "restart")
            print("Demo is running; existing sample edits are preserved. Use check for strict sample acceptance.")
            show(runtime, state)
        elif args.action == "stop":
            compose(runtime, state, "stop", timeout=120)
            state["phase"] = "stopped"
            write_state(runtime, state)
            print("Stopped. Docker volumes, sample files and login credentials are retained. Run start to resume.")
        elif args.action == "check":
            check_services(service_rows(runtime, state))
            if not state.get("verified_at"):
                raise DemoError("No completed sample verification exists. Run start.")
            verify_gateway()
            tools_run(runtime, state, "--check")
            print("Current service and saved sample checks passed.")
            show(runtime, state)
        else:
            for row in service_rows(runtime, state):
                print(f"{row.get('Service', '?')}: {row.get('State', '?')} {row.get('Health', '')}")
            print("Last completed sample verification: " + state.get("verified_at", "never"))
            show(runtime, state)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (DemoError, OSError, ValueError, KeyError) as error:
        print("Demo not ready: " + (str(error) if isinstance(error, DemoError) else "runtime operation failed; no credentials are printed."), file=sys.stderr)
        raise SystemExit(1)
    except KeyboardInterrupt:
        print("Interrupted. Retained data was not deleted; run start to resume.", file=sys.stderr)
        raise SystemExit(130)
