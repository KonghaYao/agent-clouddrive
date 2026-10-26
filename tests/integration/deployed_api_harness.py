"""Real Keycloak PKCE and REST helpers. Importing this module performs no I/O.

Only disposable accounts, no fake bearer tokens, no TLS bypass, and no automatic
redirects for authenticated API or token requests. Exception text is deliberately
static: upstream bodies, URLs, usernames, cookies and tokens must never be logged.
"""
from __future__ import annotations

import base64
from dataclasses import dataclass, field
import hashlib
from html.parser import HTMLParser
import http.cookiejar
import ipaddress
import json
import os
import re
from pathlib import Path
import secrets
import ssl
import stat
import time
import urllib.error
import urllib.parse
import urllib.request

SCOPES = "openid drive.read drive.write drive.delete drive.restore drive.convert drive.share.request drive.feedback"


class DriverError(Exception):
    """Safe local diagnostic code, never raw dependency exception text."""


def require(condition: bool, code: str) -> None:
    if not condition:
        raise DriverError(code)


def origin(url: str) -> str:
    p = urllib.parse.urlsplit(url)
    return f"{p.scheme}://{p.netloc}"


def validate_url(value: str, *, loopback_http: bool = False, base: bool = False) -> str:
    try:
        p = urllib.parse.urlsplit(value)
        port = p.port
    except ValueError:
        raise DriverError("invalid_endpoint") from None
    require(bool(p.hostname) and not p.username and not p.password and not p.query and not p.fragment,
            "invalid_endpoint")
    require(not any(c.isspace() or ord(c) < 32 for c in value) and "\\" not in value, "invalid_endpoint")
    require(port is None or 1 <= port <= 65535, "invalid_endpoint_port")
    if p.scheme != "https":
        try:
            loopback = ipaddress.ip_address(p.hostname).is_loopback
        except ValueError:
            # Exact localhost only. Arbitrary *.localhost is not assumed to resolve locally.
            loopback = p.hostname == "localhost"
        require(p.scheme == "http" and loopback_http and loopback, "https_required")
    require(not base or p.path in ("", "/"), "public_url_requires_origin")
    require(not any(s in (".", "..") for s in p.path.split("/")) and "%" not in p.path,
            "invalid_endpoint_path")
    return value.rstrip("/")


def protected_credentials(path: Path, need_reader: bool) -> dict:
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "r", encoding="utf-8") as f:
            st = os.fstat(f.fileno())
            require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077,
                    "credentials_require_owned_private_regular_file")
            require(st.st_size <= 65536, "credentials_too_large")
            data = json.load(f)
    except DriverError:
        raise
    except (OSError, ValueError, UnicodeError):
        raise DriverError("credentials_unreadable_or_invalid") from None
    require(isinstance(data, dict), "invalid_fixture_credentials")
    for role in (("owner", "reader") if need_reader else ("owner",)):
        account = data.get(role)
        require(isinstance(account, dict), "missing_fixture_account")
        require(all(isinstance(account.get(k), str) and 0 < len(account[k]) <= 4096
                    for k in ("username", "password")), "invalid_fixture_account")
    if need_reader:
        require(data["owner"]["username"] != data["reader"]["username"], "distinct_fixture_users_required")
    return data


def protected_search_key(path: Path) -> str:
    """Optional raw Meilisearch test key; never read it from CLI arguments/logs."""
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        with os.fdopen(fd, "r", encoding="utf-8") as f:
            st = os.fstat(f.fileno())
            require(stat.S_ISREG(st.st_mode) and st.st_uid == os.getuid() and not st.st_mode & 0o077,
                    "search_key_requires_owned_private_regular_file")
            require(st.st_size <= 4096, "search_key_too_large")
            value = f.read().strip()
            require(bool(value) and not any(c.isspace() or ord(c) < 32 for c in value), "invalid_search_key_file")
            return value
    except DriverError:
        raise
    except (OSError, ValueError, UnicodeError):
        raise DriverError("search_key_unreadable_or_invalid") from None


@dataclass(repr=False)
class Config:
    public: str
    issuer: str
    credentials: dict = field(repr=False)
    ca_file: str | None = None
    timeout: int = 120
    client_id: str = "clouddrive-web"
    meili_url: str | None = None
    meili_index: str = "clouddrive"
    meili_key: str = field(default="", repr=False)

    @classmethod
    def from_environment(cls, env: dict, loopback_http: bool, need_reader: bool) -> Config:
        require(all(env.get(k) for k in ("TEST_PUBLIC_URL", "TEST_OIDC_ISSUER", "TEST_CREDENTIALS_FILE")),
                "missing_required_configuration")
        public = validate_url(env["TEST_PUBLIC_URL"], loopback_http=loopback_http, base=True)
        issuer = validate_url(env["TEST_OIDC_ISSUER"], loopback_http=loopback_http)
        require("/realms/" in urllib.parse.urlsplit(issuer).path, "keycloak_realm_issuer_required")
        creds = protected_credentials(Path(env["TEST_CREDENTIALS_FILE"]), need_reader)
        ca = env.get("TEST_CA_FILE") or None
        try:
            ssl.create_default_context(cafile=ca)
        except (OSError, ssl.SSLError):
            raise DriverError("invalid_ca_file") from None
        meili_url = env.get("TEST_MEILI_URL") or None
        require(bool(meili_url) or not (env.get("TEST_MEILI_INDEX") or env.get("TEST_MEILI_KEY_FILE")),
                "search_endpoint_required_for_optional_index_configuration")
        meili_key = ""
        meili_index = env.get("TEST_MEILI_INDEX", "clouddrive")
        if meili_url:
            meili_url = validate_url(meili_url, loopback_http=loopback_http, base=True)
            require(bool(re.fullmatch(r"[A-Za-z0-9_-]{1,128}", meili_index)), "invalid_search_index_name")
            if env.get("TEST_MEILI_KEY_FILE"):
                meili_key = protected_search_key(Path(env["TEST_MEILI_KEY_FILE"]))
        return cls(public, issuer, creds, ca, meili_url=meili_url, meili_index=meili_index, meili_key=meili_key)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


@dataclass(repr=False)
class Response:
    status: int
    headers: object
    body: bytes = field(repr=False)

    def json(self) -> dict:
        try:
            value = json.loads(self.body)
        except (ValueError, UnicodeError):
            raise DriverError("invalid_json_response") from None
        require(isinstance(value, dict), "expected_json_object")
        return value


class Transport:
    def __init__(self, config: Config):
        self.config = config
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect(),
            urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=config.ca_file)),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )

    def request(self, method: str, url: str, body=None, headers=None) -> Response:
        hdr = dict(headers or {})
        if isinstance(body, dict):
            hdr["Content-Type"] = "application/json"
            body = json.dumps(body).encode()
        try:
            req = urllib.request.Request(url, data=body, method=method, headers=hdr)
            try:
                resp = self.opener.open(req, timeout=self.config.timeout)
            except urllib.error.HTTPError as e:
                resp = e
            with resp:
                data = resp.read((8 << 20) + 1)
                require(len(data) <= 8 << 20, "response_limit_exceeded")
                return Response(resp.status, resp.headers, data)
        except DriverError:
            raise
        except (OSError, ValueError, urllib.error.URLError):
            raise DriverError("network_or_tls_failure_outcome_unconfirmed") from None

    def form(self, url: str, values: dict) -> Response:
        return self.request("POST", url, urllib.parse.urlencode(values).encode(),
                            {"Content-Type": "application/x-www-form-urlencoded"})


class Forms(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.forms = []
        self.current = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "form":
            self.current = {"action": a.get("action", ""), "method": a.get("method", "get").lower(),
                            "fields": {}, "password": False, "names": set()}
            self.forms.append(self.current)
        elif tag == "input" and self.current is not None:
            name = a.get("name", "")
            self.current["names"].add(name)
            if a.get("type", "").lower() == "password":
                self.current["password"] = True
            if a.get("type", "").lower() == "hidden" and name:
                self.current["fields"][name] = a.get("value", "")

    def handle_endtag(self, tag):
        if tag == "form":
            self.current = None


def callback_code(url: str, callback: str, state: str) -> str:
    p = urllib.parse.urlsplit(url)
    target = urllib.parse.urlsplit(callback)
    require((p.scheme, p.netloc, p.path) == (target.scheme, target.netloc, target.path) and not p.fragment,
            "unexpected_oauth_callback")
    args = urllib.parse.parse_qs(p.query, keep_blank_values=True)
    require(args.get("state") == [state], "oauth_state_mismatch")
    require("error" not in args and len(args.get("code", [])) == 1 and bool(args["code"][0]),
            "oauth_authorization_not_completed")
    return args["code"][0]


def safe_id(value) -> str:
    import uuid
    try:
        require(isinstance(value, str) and str(uuid.UUID(value)) == value.lower(), "invalid_resource_id")
    except (ValueError, AttributeError):
        raise DriverError("invalid_resource_id") from None
    return value


class API:
    def __init__(self, config: Config, role: str = "owner"):
        self.config = config
        self.account = config.credentials[role]
        self.http = Transport(config)
        self.access = ""
        self.refresh = ""
        self.expires = 0.0
        self.token_endpoint = config.issuer + "/protocol/openid-connect/token"

    def _issuer_url(self, url: str) -> bool:
        p = urllib.parse.urlsplit(url)
        return (origin(url) == origin(self.config.issuer) and not p.username and not p.password
                and not p.fragment and p.path.startswith(urllib.parse.urlsplit(self.config.issuer).path + "/")
                and not any(s in (".", "..") for s in p.path.split("/")) and "%" not in p.path)

    def authorization(self, url: str, callback: str, state: str) -> str:
        """Follow only configured-realm redirects; submit only login/consent forms."""
        require(self._issuer_url(url), "untrusted_authorization_endpoint")
        response = self.http.request("GET", url)
        sent_password = False
        for _ in range(12):
            if response.status in (301, 302, 303, 307, 308):
                next_url = urllib.parse.urljoin(url, response.headers.get("Location", ""))
                p = urllib.parse.urlsplit(next_url)
                if origin(next_url) == origin(callback) and p.path == urllib.parse.urlsplit(callback).path:
                    callback_code(next_url, callback, state)
                    return next_url
                require(self._issuer_url(next_url), "untrusted_authorization_redirect")
                url = next_url
                response = self.http.request("GET", url)
                continue
            require(response.status == 200, "authorization_http_failure")
            forms = Forms()
            try:
                forms.feed(response.body.decode("utf-8"))
            except UnicodeError:
                raise DriverError("invalid_authorization_page") from None
            matches = []
            for form in forms.forms:
                action = urllib.parse.urljoin(url, form["action"])
                path = urllib.parse.urlsplit(action).path
                login = form["password"] and {"username", "password"}.issubset(form["names"])
                consent = path == urllib.parse.urlsplit(self.config.issuer).path + "/login-actions/consent" and not form["password"]
                if form["method"] == "post" and self._issuer_url(action) and (login or consent):
                    matches.append((form, action, login))
            require(len(matches) == 1, "unsupported_login_mfa_or_consent_page")
            form, action, login = matches[0]
            values = dict(form["fields"])
            if login:
                require(not sent_password and urllib.parse.urlsplit(action).path == urllib.parse.urlsplit(self.config.issuer).path + "/login-actions/authenticate",
                        "login_rejected_or_unexpected_form")
                values.update(username=self.account["username"], password=self.account["password"], credentialId="")
                sent_password = True
            else:
                values["accept"] = "Yes"
            url = action
            response = self.http.form(action, values)
        raise DriverError("authorization_redirect_limit")

    def _tokens(self, response: Response):
        require(response.status == 200, "token_exchange_failed")
        data = response.json()
        require(isinstance(data.get("access_token"), str) and bool(data["access_token"]), "access_token_missing")
        require(data.get("token_type", "").lower() == "bearer", "unexpected_token_type")
        ttl = data.get("expires_in")
        require(isinstance(ttl, (float, int)) and not isinstance(ttl, bool) and 30 < ttl <= 86400,
                "unexpected_token_expiry")
        self.access = data["access_token"]
        self.refresh = data.get("refresh_token") or self.refresh
        self.expires = time.monotonic() + ttl

    def login(self, scopes: str = SCOPES):
        discovery = self.http.request("GET", self.config.issuer + "/.well-known/openid-configuration")
        require(discovery.status == 200, "oidc_discovery_failed")
        doc = discovery.json()
        require(doc.get("issuer") == self.config.issuer, "oidc_issuer_mismatch")
        authorization_endpoint = self.config.issuer + "/protocol/openid-connect/auth"
        require(doc.get("authorization_endpoint") == authorization_endpoint and doc.get("token_endpoint") == self.token_endpoint,
                "oidc_endpoint_mismatch")
        verifier = secrets.token_urlsafe(48)
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
        state = secrets.token_urlsafe(32)
        callback = self.config.public + "/auth/callback"
        url = authorization_endpoint + "?" + urllib.parse.urlencode({
            "client_id": self.config.client_id, "redirect_uri": callback, "response_type": "code", "scope": scopes,
            "state": state, "code_challenge": challenge, "code_challenge_method": "S256",
        })
        location = self.authorization(url, callback, state)
        self._tokens(self.http.form(self.token_endpoint, {
            "grant_type": "authorization_code", "client_id": self.config.client_id,
            "redirect_uri": callback, "code": callback_code(location, callback, state), "code_verifier": verifier,
        }))
        me = self.json("GET", "/v1/me")
        require(isinstance(me.get("scopes"), list) and set(scopes.split()) - {"openid"} <= set(me["scopes"]),
                "granted_scopes_missing")
        return me

    def request(self, method: str, path: str, body=None, headers=None) -> Response:
        require(path.startswith("/v1/") and not path.startswith("//") and "#" not in path,
                "invalid_api_path")
        require(bool(self.access), "login_required")
        if time.monotonic() + 30 >= self.expires:
            require(bool(self.refresh), "session_expired_reauthorization_required")
            self._tokens(self.http.form(self.token_endpoint, {
                "grant_type": "refresh_token", "client_id": self.config.client_id, "refresh_token": self.refresh,
            }))
        hdr = {"Authorization": "Bearer " + self.access, "Origin": self.config.public}
        hdr.update(headers or {})
        return self.http.request(method, self.config.public + path, body, hdr)

    def json(self, method: str, path: str, body=None, headers=None) -> dict:
        response = self.request(method, path, body, headers)
        require(response.status == 200, "unexpected_api_http_status_" + str(response.status))
        return response.json()

    def delegate(self, *, allow_new_grant: bool = False):
        if self.json("GET", "/v1/delegation/status").get("enabled") is True:
            return
        require(allow_new_grant, "explicit_disposable_offline_consent_required")
        start = self.json("POST", "/v1/delegation/start", {})
        url = start.get("authorization_url", "")
        require(isinstance(url, str) and self._issuer_url(url), "untrusted_delegation_endpoint")
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(url).query)
        callback = self.config.public + "/v1/delegation/callback"
        require(query.get("redirect_uri") == [callback] and len(query.get("state", [])) == 1
                and bool(query["state"][0]) and query.get("response_type") == ["code"]
                and query.get("code_challenge_method") == ["S256"]
                and len(query.get("code_challenge", [])) == 1 and len(query["code_challenge"][0]) == 43
                and len(query.get("scope", [])) == 1
                and set(query["scope"][0].split()) == {"openid", "offline_access", "drive.read"}
                and urllib.parse.urlsplit(url).path == urllib.parse.urlsplit(self.config.issuer).path + "/protocol/openid-connect/auth",
                "invalid_delegation_authorization")
        location = self.authorization(url, callback, query["state"][0])
        response = self.http.request("GET", location)
        require(response.status == 303, "delegation_callback_failed")
        require(self.json("GET", "/v1/delegation/status").get("enabled") is True, "delegation_not_enabled")
