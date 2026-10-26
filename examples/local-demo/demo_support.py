"""Private disposable-demo helpers; no network or state changes at import time.

The integration harness keeps its HTTPS/exact-localhost policy unchanged. This
module adds a deliberately separate pinned-address transport for this example.
"""
from __future__ import annotations
import base64
from dataclasses import dataclass, field
import hashlib
import http.client
import http.cookiejar
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import socket
import stat
import ssl
import sys
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))
from deployed_api_harness import (API, Config, DriverError, NoRedirect, SCOPES, Transport,
                                  callback_code, origin, protected_credentials, require, safe_id)
WEB_SCOPES = "openid profile " + SCOPES.removeprefix("openid ")


def private_json(path: Path, *, maximum=1 << 20):
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "r", encoding="utf-8") as f:
            s = os.fstat(f.fileno())
            require(stat.S_ISREG(s.st_mode) and s.st_uid == os.getuid() and not s.st_mode & 0o077,
                    "private_owned_regular_file_required")
            require(s.st_nlink == 1 and s.st_size <= maximum, "private_file_invalid_size_or_links")
            value = json.load(f)
        require(isinstance(value, dict), "private_json_object_required")
        return value
    except DriverError:
        raise
    except (OSError, ValueError, UnicodeError):
        raise DriverError("private_json_unreadable_or_invalid") from None


def private_write(path: Path, value):
    """Atomic, mode-0600 state. Existing links or insecure files are rejected."""
    if path.exists() or path.is_symlink():
        private_json(path)
    data = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode()
    temporary = path.parent / ("." + path.name + "." + secrets.token_hex(8))
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "wb") as f:
            f.write(data); f.flush(); os.fsync(f.fileno())
        os.replace(temporary, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try: os.fsync(fd)
        finally: os.close(fd)
    except OSError:
        raise DriverError("private_state_write_failed") from None
    finally:
        if temporary.exists(): temporary.unlink()


def jwt_claims(raw):
    """Inspect a token received directly from trusted IdP, not signature verification.

    Actual signature and authorization enforcement is done by Keycloak/oCIS/app.
    Never accept these decoded claims as substitute for successful service reads.
    """
    try:
        require(isinstance(raw, str) and len(raw) <= 65536 and len(raw.split(".")) == 3,
                "invalid_token_shape")
        payload = raw.split(".")[1]
        result = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
        require(isinstance(result, dict), "invalid_token_claims")
        return result
    except DriverError:
        raise
    except (ValueError, UnicodeError):
        raise DriverError("invalid_token_claims") from None


def audiences(claims):
    audience = claims.get("aud")
    return [audience] if isinstance(audience, str) else audience if isinstance(audience, list) else []


@dataclass(repr=False)
class Runtime:
    root: Path
    state: dict
    config: Config = field(repr=False)
    container_network: bool = False
    native_origin: str = "http://ocis:9200"

    def native_http(self):
        return DemoTransport(self, native=True)

    @property
    def issuer(self): return self.config.issuer

    @property
    def public(self): return self.config.public

    def api(self, role="owner"):
        return DemoAPI(self, role)


def load_runtime(path, container_network=False):
    root = Path(path).absolute()
    require(not any(part.is_symlink() for part in (root, *root.parents)), "runtime_symlink_component_forbidden")
    try:
        s = root.lstat()
        require(stat.S_ISDIR(s.st_mode) and not stat.S_ISLNK(s.st_mode)
                and s.st_uid == os.getuid() and not s.st_mode & 0o077,
                "runtime_requires_owned_private_directory")
        require(not root.is_relative_to(ROOT), "runtime_must_be_outside_source_tree")
    except OSError:
        raise DriverError("runtime_unavailable") from None
    state = private_json(root / "demo-state.json")
    require(state.get("schema") == 1 and state.get("kind") == "agent-clouddrive-local-demo"
            and isinstance(state.get("project"), str) and re.fullmatch(r"[a-z][a-z0-9-]{3,63}", state["project"]),
            "not_a_disposable_demo_runtime")
    public = state.get("public_url", "")
    try:
        p = urllib.parse.urlsplit(public)
        require(p.scheme == "http" and p.hostname in ("drive.localhost", "localhost", "127.0.0.1", "::1")
                and p.port is not None and 1024 <= p.port <= 65535 and p.path == ""
                and not p.query and not p.fragment and not p.username and not p.password,
                "invalid_demo_public_origin")
        require(not any(c.isspace() or ord(c) < 32 for c in public) and "\\" not in public,
                "invalid_demo_public_origin")
    except (ValueError, TypeError):
        raise DriverError("invalid_demo_public_origin") from None
    credentials = protected_credentials(root / "credentials.json", True)
    for role in ("owner", "reader"):
        require(re.fullmatch(role + r"-[0-9a-f]{16}", credentials[role]["username"]) is not None,
                "synthetic_demo_user_required")
    config = Config(public, public + "/identity/realms/clouddrive", credentials, timeout=120)
    return Runtime(root, state, config, container_network)


def pinned_addresses(host, port, container_network):
    try:
        resolved = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        addresses = list(dict.fromkeys(item[4][0] for item in resolved))
        require(bool(addresses), "demo_host_did_not_resolve")
        for address in addresses:
            ip = ipaddress.ip_address(address)
            allowed = ip.is_loopback
            if container_network:
                # RFC1918/ULA only, not link-local, multicast, unspecified or public.
                allowed = allowed or any(ip in network for network in (
                    ipaddress.ip_network("10.0.0.0/8"), ipaddress.ip_network("172.16.0.0/12"),
                    ipaddress.ip_network("192.168.0.0/16"), ipaddress.ip_network("fc00::/7"))
                    if ip.version == network.version)
            require(allowed, "demo_host_not_in_allowed_local_network")
        return addresses
    except DriverError:
        raise
    except (OSError, ValueError):
        raise DriverError("demo_host_resolution_failed") from None


class PinnedHTTP(urllib.request.HTTPHandler):
    def __init__(self, container_network):
        super().__init__(); self.container_network = container_network

    def http_open(self, req):
        parsed = urllib.parse.urlsplit(req.full_url)
        addresses = pinned_addresses(parsed.hostname, parsed.port or 80, self.container_network)
        def connection(host, **kwargs):
            conn = http.client.HTTPConnection(host, **kwargs)
            # Host header and cookie origin stay unchanged; connect never re-resolves.
            def connect(address, timeout=socket._GLOBAL_DEFAULT_TIMEOUT, source_address=None):
                last = None
                for address_ip in addresses:
                    try: return socket.create_connection((address_ip, parsed.port or 80), timeout, source_address)
                    except OSError as e: last = e
                raise OSError("pinned_connection_failed") from last
            conn._create_connection = connect
            return conn
        return self.do_open(connection, req)


class PinnedHTTPS(urllib.request.HTTPSHandler):
    def __init__(self, config):
        self.context = ssl.create_default_context(cafile=config.ca_file)
        super().__init__(context=self.context)

    def https_open(self, req):
        parsed = urllib.parse.urlsplit(req.full_url)
        addresses = pinned_addresses(parsed.hostname, parsed.port or 443, False)
        def connection(host, **kwargs):
            conn = http.client.HTTPSConnection(host, context=self.context, **kwargs)
            def connect(address, timeout=socket._GLOBAL_DEFAULT_TIMEOUT, source_address=None):
                for ip in addresses:
                    try: return socket.create_connection((ip, parsed.port or 443), timeout, source_address)
                    except OSError: pass
                raise OSError("pinned_connection_failed")
            conn._create_connection = connect
            return conn
        return self.do_open(connection, req)


class DemoTransport(Transport):
    def __init__(self, runtime, *, native=False):
        self.config = runtime.config
        self.origins = {runtime.public, origin(runtime.issuer)}
        if native:
            if runtime.container_network:
                require(runtime.native_origin == "http://ocis:9200", "invalid_container_native_origin")
                self.origins.update((runtime.native_origin, "http://keycloak:8080"))
            else:
                native = urllib.parse.urlsplit(runtime.native_origin)
                require(native.scheme == "https" and native.hostname in ("localhost", "127.0.0.1", "::1")
                        and native.path == "" and not native.query and not native.fragment
                        and not native.username and not native.password and bool(runtime.config.ca_file),
                        "native_proof_requires_loopback_tls_and_explicit_ca")
                self.origins.add(runtime.native_origin)
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(),
            PinnedHTTP(runtime.container_network), PinnedHTTPS(runtime.config), urllib.request.HTTPCookieProcessor(self.cookies))

    def request(self, method, url, body=None, headers=None):
        try: p = urllib.parse.urlsplit(url)
        except ValueError: raise DriverError("untrusted_demo_endpoint") from None
        decoded_path = urllib.parse.unquote(p.path)
        require("%" not in decoded_path and "\\" not in decoded_path
                and not any(ord(c) < 32 for c in decoded_path)
                and not any(s in (".", "..") for s in decoded_path.split("/")), "untrusted_demo_endpoint")
        require(origin(url) in self.origins and p.scheme in ("http", "https") and not p.username and not p.password
                and not p.fragment and not any(c.isspace() or ord(c) < 32 for c in url) and "\\" not in url
                and not any(s in (".", "..") for s in p.path.split("/")), "untrusted_demo_endpoint")
        return super().request(method, url, body, headers)


class DemoAPI(API):
    def __init__(self, runtime, role="owner"):
        super().__init__(runtime.config, role)
        self.http = DemoTransport(runtime)
        self.profile = None

    def oauth_login(self):
        discovery = self.http.request("GET", self.config.issuer + "/.well-known/openid-configuration")
        require(discovery.status == 200, "oidc_discovery_failed")
        doc = discovery.json()
        authorization_endpoint = self.config.issuer + "/protocol/openid-connect/auth"
        require(doc.get("issuer") == self.config.issuer and doc.get("authorization_endpoint") == authorization_endpoint
                and doc.get("token_endpoint") == self.token_endpoint, "oidc_discovery_mismatch")
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
        state, nonce = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
        callback = self.config.public + "/auth/callback"
        location = self.authorization(authorization_endpoint + "?" + urllib.parse.urlencode({
            "client_id": self.config.client_id, "redirect_uri": callback, "response_type": "code", "scope": WEB_SCOPES,
            "state": state, "nonce": nonce, "code_challenge": challenge, "code_challenge_method": "S256"}), callback, state)
        response = self.http.form(self.token_endpoint, {"grant_type": "authorization_code", "client_id": self.config.client_id,
            "redirect_uri": callback, "code": callback_code(location, callback, state), "code_verifier": verifier})
        self._tokens(response)
        tokens = response.json()
        profile = jwt_claims(tokens.get("id_token"))
        access = jwt_claims(self.access)
        require(profile.get("iss") == self.config.issuer and profile.get("nonce") == nonce
                and self.config.client_id in audiences(profile) and profile.get("sub") == access.get("sub")
                and profile.get("preferred_username") == self.account["username"]
                and isinstance(profile.get("name"), str) and bool(profile["name"])
                and access.get("iss") == self.config.issuer and "agent-clouddrive" in audiences(access),
                "web_profile_or_token_contract_failed")
        require(set(WEB_SCOPES.split()) <= set(access.get("scope", "").split()), "web_scopes_missing")
        self.profile = profile
        return profile

    def login(self):
        self.oauth_login()
        me = self.json("GET", "/v1/me")
        safe_id(me.get("id"))
        require(me.get("actor_type") == "user" and isinstance(me.get("scopes"), list)
                and set(SCOPES.split()) - {"openid"} <= set(me["scopes"]), "business_identity_contract_failed")
        response = self.request("POST", "/v1/session", {})
        require(response.status == 200, "web_session_not_established")
        data = response.json()
        require(isinstance(data.get("csrf_token"), str) and len(data["csrf_token"]) >= 32
                and "session_token" not in data, "web_session_response_invalid")
        session = [cookie for cookie in self.http.cookies if cookie.name == "drive_session"]
        require(len(session) == 1 and session[0].path == "/" and not session[0].secure
                and session[0].has_nonstandard_attr("HttpOnly")
                and str(session[0].get_nonstandard_attr("SameSite", "")).lower() == "strict",
                "web_session_cookie_contract_failed")
        return me
