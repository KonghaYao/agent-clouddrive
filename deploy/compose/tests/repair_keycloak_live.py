"""在现有本机 Keycloak 创建隔离 realm，运行真实事件+PG 测试后删除。
只输出测试日志/HTTP 状态；凭据仅在进程内与子进程环境中传递。
"""
import json
import os
from pathlib import Path
import secrets
import subprocess
import urllib.error
import urllib.parse
import urllib.request
import uuid


def main():
    base = os.environ.get("REPAIR_KEYCLOAK_BASE", "http://127.0.0.1:8080/identity")
    container = os.environ.get("REPAIR_KEYCLOAK_CONTAINER", "clouddrive-demo-df68102d15f5-keycloak-1")
    password = subprocess.check_output(["docker", "exec", container, "cat", "/run/secrets/keycloak_admin_password"], text=True).strip()

    def call(method, path, body=None, token=None, form=False):
        data = urllib.parse.urlencode(body).encode() if form else (json.dumps(body).encode() if body is not None else None)
        req = urllib.request.Request(base + path, data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/x-www-form-urlencoded" if form else "application/json")
        if token:
            req.add_header("Authorization", "Bearer " + token)
        try:
            with urllib.request.urlopen(req, timeout=30) as response:
                value = response.read()
                return json.loads(value) if value else None
        except urllib.error.HTTPError as error:
            raise RuntimeError("Keycloak HTTP " + str(error.code)) from None

    token = call("POST", "/realms/master/protocol/openid-connect/token", {"grant_type": "password", "client_id": "admin-cli", "username": "bootstrap-admin", "password": password}, form=True)["access_token"]
    password = None
    name = "repair-e-" + uuid.uuid4().hex
    secret = secrets.token_urlsafe(32)
    admin = "/admin/realms/" + name
    call("POST", "/admin/realms", {"realm": name, "enabled": True, "sslRequired": "none", "adminEventsEnabled": True, "adminEventsDetailsEnabled": True, "eventsEnabled": True, "eventsListeners": [], "clients": [{"clientId": "repair-identity", "secret": secret, "publicClient": False, "serviceAccountsEnabled": True, "fullScopeAllowed": True, "standardFlowEnabled": False}], "users": [{"username": "owner", "enabled": True}]}, token)
    try:
        clients = call("GET", admin + "/clients", token=token)
        ids = {item["clientId"]: item["id"] for item in clients}
        service = call("GET", admin + "/clients/" + ids["repair-identity"] + "/service-account-user", token=token)["id"]
        roles = [call("GET", admin + "/clients/" + ids["realm-management"] + "/roles/" + role, token=token) for role in ("view-users", "view-events")]
        call("POST", admin + "/users/" + service + "/role-mappings/clients/" + ids["realm-management"], roles, token)
        owner = call("GET", admin + "/users?username=owner&exact=true", token=token)[0]["id"]
        env = os.environ.copy()
        env.update({"GOTOOLCHAIN": "go1.25.0", "REPAIR_KEYCLOAK_ISSUER": base + "/realms/" + name, "REPAIR_KEYCLOAK_SUBJECT": owner, "REPAIR_KEYCLOAK_CLIENT": "repair-identity", "REPAIR_KEYCLOAK_SECRET": secret, "REPAIR_KEYCLOAK_ADMIN_TOKEN": token, "REPAIR_KEYCLOAK_ADMIN_URL": base + admin})
        result = subprocess.run(["go", "test", "-race", "-count=1", "-run", "TestRepairIdentityLiveKeycloak$", "-v", "./internal/application"], cwd=Path(__file__).resolve().parents[3], env=env)
        return result.returncode
    finally:
        call("DELETE", admin, token=token)
        print("隔离 Keycloak realm 已删除；未修改 demo realm。")


if __name__ == "__main__":
    raise SystemExit(main())
