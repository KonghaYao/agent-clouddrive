#!/usr/bin/env python3
"""Prepare idempotent local configuration. Does not deploy or migrate storage."""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import sys
import urllib.parse
from keycloak.realm_template import realm

HERE = Path(__file__).resolve().parent


def write(path, content, mode=0o444):
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.read_text() == content:
            return
        path.chmod(0o600)
    path.write_text(content)
    path.chmod(mode)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=["local", "s3"], default="s3")
    parser.add_argument("--public-url", default="http://drive.localhost:8080")
    parser.add_argument("--development-http", action="store_true", help="explicitly allow HTTP ONLY on loopback .localhost; never production")
    parser.add_argument("--directory", type=Path, default=HERE, help="configuration output directory")
    parser.add_argument("--tls-cert", type=Path)
    parser.add_argument("--tls-key", type=Path)
    args = parser.parse_args()
    base = args.public_url.rstrip("/")
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.path or parsed.query or parsed.fragment or parsed.username:
        parser.error("public URL must be an http(s) origin without path, query or credentials")
    if parsed.hostname == "localhost" or parsed.hostname.replace(".", "").isdigit():
        parser.error("use drive.localhost for local Docker DNS, or a DNS hostname for production")
    local_http = parsed.scheme == "http" and parsed.hostname.endswith(".localhost") and args.development_http
    if (parsed.port or (443 if parsed.scheme == "https" else 80)) != (443 if parsed.scheme == "https" else 8080):
        parser.error("reference gateway requires HTTP :8080 or HTTPS :443 so the public issuer resolves to the same internal listener")
    if parsed.scheme == "http" and not local_http:
        parser.error("HTTP is only permitted with --development-http and a .localhost origin")
    if parsed.scheme == "https" and (not args.tls_cert or not args.tls_key):
        parser.error("HTTPS needs --tls-cert and --tls-key from a trusted CA; never bypass browser security warnings")
    target = args.directory.resolve()
    target.mkdir(parents=True, exist_ok=True)
    statefile = target / ".deployment-state.json"
    if statefile.exists():
        old = json.loads(statefile.read_text())
        if old["storage_mode"] != args.mode:
            parser.error("storage mode change is not a migration; initialize a new project or perform a separately reviewed migration")
        if old["public_url"] != base:
            parser.error("issuer changes require a reviewed Keycloak/client migration; use the original public URL")
    secret_dir, generated = target / "secrets", target / "generated"
    secret_dir.mkdir(mode=0o700, exist_ok=True); secret_dir.chmod(0o700)
    generated.mkdir(mode=0o700, exist_ok=True); generated.chmod(0o700)
    names = ("postgres_password", "keycloak_db_password", "keycloak_admin_password", "meili_master_key", "nats_token", "s3_access_key", "s3_secret_key", "converter_token", "backend_client_secret", "identity_client_secret", "background_client_secret", "ocis_admin_password")
    values = {}
    for name in names:
        path = secret_dir / name
        if not path.exists():
            write(path, secrets.token_urlsafe(32) + "\n")
        values[name] = path.read_text().strip()
    if not (secret_dir / "delegation_key").exists():
        (secret_dir / "delegation_key").write_bytes(secrets.token_bytes(32))
        (secret_dir / "delegation_key").chmod(0o444)
    # Files are readable by the receiving non-root container UID. Parent dirs
    # remain 0700 so other host users cannot traverse; mount individual files.
    write(secret_dir / "drive_database_url", "postgres://clouddrive:" + urllib.parse.quote(values["postgres_password"], safe="") + "@postgres:5432/clouddrive?sslmode=disable\n")
    write(secret_dir / "drive_nats_url", "nats://" + values["nats_token"] + "@nats:4222\n")
    write(secret_dir / "identity_service_credential.json", json.dumps({"client_id": "clouddrive-identity", "client_secret": values["identity_client_secret"], "exchange_client_id": "agent-clouddrive", "exchange_client_secret": values["backend_client_secret"], "background_client_id": "clouddrive-background", "background_client_secret": values["background_client_secret"]}) + "\n")
    if not (secret_dir / "vision_api_key").exists():
        write(secret_dir / "vision_api_key", "")
    # Optional TLS secrets must exist for Compose config even in local mode;
    # nginx only loads them in HTTPS mode.
    if parsed.scheme == "https":
        for source, name in ((args.tls_cert, "tls_certificate"), (args.tls_key, "tls_private_key")):
            destination = secret_dir / name
            if destination.exists():
                destination.chmod(0o600)
            shutil.copyfile(source, destination); destination.chmod(0o444)
    else:
        for name in ("tls_certificate", "tls_private_key"):
            if not (secret_dir / name).exists():
                write(secret_dir / name, "")
    issuer = base + "/identity/realms/clouddrive"
    configuration = realm(base, values["backend_client_secret"], values["identity_client_secret"], values["background_client_secret"])
    if local_http:
        configuration["sslRequired"] = "none"
    write(generated / "clouddrive-realm.json", json.dumps(configuration, indent=2) + "\n")
    write(generated / "config.json", json.dumps({"oidc_issuer": issuer, "oidc_client_id": "clouddrive-web", "oidc_audience": "agent-clouddrive"}) + "\n")
    nginx = (HERE / "gateway" / "nginx.conf").read_text()
    if parsed.scheme == "https":
        nginx = nginx.replace("    listen 8080;", "    listen 443 ssl;\n    ssl_certificate /run/secrets/tls_certificate;\n    ssl_certificate_key /run/secrets/tls_private_key;\n    ssl_protocols TLSv1.2 TLSv1.3;")
        nginx = nginx.replace("  server {", "  server { listen 8080; location = /healthz { return 200 'ok'; } }\n  server {", 1)
    write(generated / "nginx.conf", nginx)
    env = (HERE / ".env.example").read_text().splitlines()
    replacements = {"DRIVE_STORAGE_MODE": args.mode, "STORAGE_USERS_DRIVER": "ocis" if args.mode == "local" else "s3ng", "DRIVE_PUBLIC_URL": base,
                    "DRIVE_MCP_PUBLIC_URL": base + "/mcp", "DRIVE_ALLOWED_ORIGINS": base, "DRIVE_OIDC_ISSUER": issuer, "DRIVE_HOSTNAME": parsed.hostname,
                    "DRIVE_PORT": str(parsed.port or (443 if parsed.scheme == "https" else 80)), "DRIVE_BIND_ADDRESS": "127.0.0.1" if local_http else "0.0.0.0"}
    # Preserve operator-edited unrelated settings across re-running setup.
    if (target / ".env").exists():
        env = (target / ".env").read_text().splitlines()
    for index, line in enumerate(env):
        key = line.split("=", 1)[0]
        if key in replacements:
            env[index] = key + "=" + replacements[key]
    env = [line for line in env if not line.startswith("DRIVE_TARGET_PORT=")]
    env.append("DRIVE_TARGET_PORT=" + ("443" if parsed.scheme == "https" else "8080"))
    write(target / ".env", "\n".join(env) + "\n", 0o600)
    write(statefile, json.dumps({"storage_mode": args.mode, "public_url": base, "development_http": local_http}) + "\n", 0o600)
    print("Configuration prepared. Secrets unchanged on repeated runs; no containers started.")
    print("Local HTTP development mode only." if local_http else "TLS enabled; certificate/key files configured.")
    print("Review .env, then run ./up.sh. Keep secrets/ and generated/ out of source control and ZIP delivery.")


if __name__ == "__main__":
    main()
