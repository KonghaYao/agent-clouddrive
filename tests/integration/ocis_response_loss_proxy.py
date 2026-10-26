#!/usr/bin/env python3
"""Disposable A23 preparation: drop one real successful native MOVE response.

This is NOT an acceptance driver. It never generates success responses for MOVE,
retries a mutation, follows redirects, or treats proxy-unit tests as live evidence.
Import and --check-only perform no network I/O. See fault-injection-acceptance.md.
"""
from __future__ import annotations

import argparse
from dataclasses import dataclass, field
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import ipaddress
import json
import os
import re
import socket
import ssl
import sys
import threading
from urllib.parse import unquote_to_bytes, urlsplit


class ProxyError(Exception):
    """Only static diagnostic codes belong in this exception."""


def require(value, code):
    if not value:
        raise ProxyError(code)


def origin(value, *, allow_loopback_http=False):
    try:
        p = urlsplit(value)
        port = p.port
        local = p.hostname == "localhost" or ipaddress.ip_address(p.hostname or "").is_loopback
    except ValueError:
        try:
            p = urlsplit(value)
            port = p.port
            local = False
        except ValueError:
            raise ProxyError("invalid_origin") from None
    require(p.hostname and p.username is None and p.password is None and not p.query and not p.fragment
            and p.path in ("", "/") and "\\" not in value
            and not any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value), "invalid_origin")
    require(p.scheme == "https" or (allow_loopback_http and local and p.scheme == "http"), "https_required")
    require(port is None or 1 <= port <= 65535, "invalid_port")
    return value.rstrip("/")


def request_target(value):
    require(isinstance(value, str) and len(value) <= 8192, "invalid_request_target")
    require(value.startswith("/") and not value.startswith("//") and "\\" not in value
            and not any(c.isspace() or ord(c) < 32 or ord(c) >= 127 for c in value), "invalid_request_target")
    try:
        p = urlsplit(value)
        require(not p.scheme and not p.netloc and not p.fragment, "invalid_request_target")
        require(not re.search(r"%(?![a-fA-F0-9]{2})", p.path), "invalid_request_target")
        decoded = unquote_to_bytes(p.path).decode("utf-8")
    except (UnicodeError, ValueError):
        raise ProxyError("invalid_request_target") from None
    require("\\" not in decoded and "%" not in decoded
            and not any(ord(c) < 32 or ord(c) == 127 for c in decoded)
            and not any(s in (".", "..", "") for s in decoded.split("/")[1:]), "invalid_request_target")
    # Escaped separators create ambiguous fixture matches and are unnecessary here.
    require(not re.search(r"%(?:2f|5c)", p.path, re.I), "invalid_request_target")
    return p


def fixture_path(value, *, destination=False):
    p = request_target(value)
    parts = p.path.split("/")
    require(not p.query and parts[:3] == ["", "dav", "spaces"]
            and len(parts) == (5 if destination else 4) and "*" not in p.path, "invalid_fixture_path")
    if destination:
        require(re.fullmatch(r"a23-[A-Za-z0-9._-]{8,120}", parts[-1]), "destination_requires_a23_fixture_name")
    return value


@dataclass(repr=False)
class Config:
    upstream: str
    app_origin: str
    move_path: str
    destination_path: str
    bind: str = "127.0.0.1"
    port: int = 18923
    timeout: float = 20
    max_body: int = 8 * 1024 * 1024
    ca_file: str | None = None
    tls_context: ssl.SSLContext = field(init=False, repr=False)

    def __post_init__(self):
        self.upstream = origin(self.upstream)
        self.app_origin = origin(self.app_origin, allow_loopback_http=True)
        self.move_path = fixture_path(self.move_path)
        self.destination_path = fixture_path(self.destination_path, destination=True)
        try:
            require(ipaddress.ip_address(self.bind).is_loopback, "loopback_bind_required")
        except ValueError:
            raise ProxyError("loopback_bind_requires_ip_literal") from None
        require(1 <= self.port <= 65535 and 1 <= self.timeout <= 120
                and 1 <= self.max_body <= 16 * 1024 * 1024, "invalid_limits")
        try:
            self.tls_context = ssl.create_default_context(cafile=self.ca_file)
        except (OSError, ssl.SSLError):
            raise ProxyError("invalid_ca_file") from None

    @classmethod
    def from_environment(cls, env):
        require(env.get("ALLOW_DISPOSABLE_TEST_FAULTS") == "yes", "disposable_fault_opt_in_required")
        keys = ("TEST_OCIS_UPSTREAM_URL", "TEST_OCIS_APP_ORIGIN", "TEST_OCIS_FAULT_MOVE_PATH",
                "TEST_OCIS_FAULT_DESTINATION_PATH")
        require(all(env.get(k) for k in keys), "missing_configuration")
        try:
            return cls(*(env[k] for k in keys), bind=env.get("TEST_OCIS_PROXY_BIND", "127.0.0.1"),
                       port=int(env.get("TEST_OCIS_PROXY_PORT", "18923")),
                       timeout=float(env.get("TEST_OCIS_PROXY_TIMEOUT", "20")),
                       max_body=int(env.get("TEST_OCIS_PROXY_MAX_BODY", str(8 * 1024 * 1024))),
                       ca_file=env.get("TEST_OCIS_CA_FILE") or None)
        except (TypeError, ValueError):
            raise ProxyError("invalid_configuration") from None


class State:
    """Single-use arm; every matching attempt is counted, including blocked retries."""
    def __init__(self):
        self.lock = threading.Lock()
        self.attempts = self.forwarded = self.succeeded = self.dropped = 0
        self.upstream_status = None

    def reserve_move(self):
        with self.lock:
            self.attempts += 1
            if self.forwarded:
                return False
            self.forwarded += 1
            return True

    def observe(self, status):
        with self.lock:
            self.upstream_status = status
            if status in (201, 204):
                self.succeeded += 1

    def dropped_response(self):
        with self.lock:
            self.dropped += 1

    def proof(self):
        with self.lock:
            return {"method": "MOVE", "status": self.upstream_status,
                    "counts": {"matching_attempts": self.attempts, "forwarded": self.forwarded,
                               "upstream_success": self.succeeded, "responses_dropped": self.dropped}}


REQUEST_HEADERS = {"authorization", "accept", "accept-encoding", "content-type", "depth", "range",
                   "if-match", "if-none-match", "if-modified-since", "if-unmodified-since"}
RESPONSE_HEADERS = {"content-type", "etag", "last-modified", "accept-ranges", "content-range", "dav"}


def one_header(headers, name, *, required=False):
    values = headers.get_all(name, [])
    require(len(values) <= 1 and (values or not required), "invalid_header")
    value = values[0] if values else ""
    require(not any(ord(c) < 32 or ord(c) == 127 for c in value), "invalid_header")
    if required:
        require(value, "missing_header")
    return value


def prepare_request(config, method, target, headers):
    """Validate before connecting; never derive the upstream host from a request."""
    p = request_target(target)
    require(method in {"GET", "HEAD", "PROPFIND", "MOVE"}, "method_not_allowed")
    require(p.path.startswith(("/dav/", "/graph/v1.0/")), "path_not_allowed")
    require(not headers.get_all("Transfer-Encoding") and not headers.get_all("Expect"), "unsupported_framing")
    length = one_header(headers, "Content-Length") or "0"
    require(re.fullmatch(r"[0-9]{1,9}", length), "invalid_content_length")
    length = int(length)
    require(length <= config.max_body and (method == "PROPFIND" or length == 0), "request_body_limit")
    one_header(headers, "Host", required=True)
    one_header(headers, "Authorization", required=True)
    outgoing = {}
    for name in REQUEST_HEADERS:
        value = one_header(headers, name)
        if value:
            outgoing[name] = value
    connection = one_header(headers, "Connection")
    require(not any(v.strip().lower() in REQUEST_HEADERS | {"destination", "overwrite", "host", "content-length"}
                    for v in connection.split(",")), "invalid_connection_header")
    if method == "MOVE":
        require(target == config.move_path, "move_outside_fixture")
        require(one_header(headers, "Destination", required=True) == config.app_origin + config.destination_path,
                "destination_outside_fixture")
        require(one_header(headers, "Overwrite", required=True) == "F", "overwrite_forbidden")
        etag = one_header(headers, "If-Match", required=True)
        require(etag.startswith('"') and etag.endswith('"') and etag.count('"') == 2, "strong_if_match_required")
        outgoing["Destination"] = config.upstream + config.destination_path
        outgoing["Overwrite"] = "F"
    elif headers.get_all("Destination"):
        raise ProxyError("destination_not_allowed")
    outgoing["Content-Length"] = str(length)
    outgoing["Connection"] = "close"
    return outgoing, length


class ProxyServer(HTTPServer):
    allow_reuse_address = False

    def __init__(self, config):
        self.config = config
        self.state = State()
        if ":" in config.bind:
            self.address_family = socket.AF_INET6
        super().__init__((config.bind, config.port), Handler)

    def handle_error(self, request, client_address):
        # Base server prints traceback (possibly containing URLs/headers) otherwise.
        pass


def deadline_socket(tls_socket):
    """A shutdown-only handle; SSLSocket.dup() itself is not supported by Python.

    This raw duplicate is never used to read, write, wrap or bypass TLS. The
    original HTTPSConnection continues doing all encrypted application I/O.
    """
    fd = os.dup(tls_socket.fileno())
    try:
        return socket.socket(fileno=fd)
    except Exception:
        os.close(fd)
        raise


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "DisposableFaultProxy"
    sys_version = ""

    def log_message(self, *args):
        pass

    def send_error(self, code, message=None, explain=None):
        # Never reflect a malformed method, target, upstream message or credentials.
        self.close_connection = True
        self.send_response_only(code)
        self.send_header("Content-Length", "0")
        self.send_header("Connection", "close")
        self.end_headers()

    def handle_expect_100(self):
        self.send_error(417)
        return False

    def handle(self):
        self.upstream_socket = None
        self.expired = threading.Event()
        self.connection.settimeout(self.server.config.timeout)
        # Hard deadline covers request-header slowloris, body, upstream and response.
        timer = threading.Timer(self.server.config.timeout, self.expire)
        timer.daemon = True
        timer.start()
        try:
            super().handle()
        finally:
            timer.cancel()

    def expire(self):
        self.expired.set()
        for conn in (self.connection, self.upstream_socket):
            if conn is not None:
                try:
                    conn.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    def forward(self):
        self.close_connection = True
        config = self.server.config
        conn = None
        try:
            # Validate the wire target too: BaseHTTPRequestHandler normalizes //.
            target = self.raw_requestline.decode("iso-8859-1").split()[1]
            outgoing, length = prepare_request(config, self.command, target, self.headers)
            body = self.rfile.read(length) if length else None
            require(body is None or len(body) == length, "incomplete_request_body")
            if self.command == "MOVE" and not self.server.state.reserve_move():
                self.send_error(409)
                return
            upstream = urlsplit(config.upstream)
            conn = http.client.HTTPSConnection(upstream.hostname, upstream.port or 443,
                                               timeout=config.timeout, context=config.tls_context)
            conn.connect()
            # Retain a duplicate so the hard deadline still works after http.client
            # detaches its socket for a Connection: close response body.
            self.upstream_socket = deadline_socket(conn.sock)
            if self.expired.is_set():
                raise TimeoutError()
            conn.request(self.command, target, body=body, headers=outgoing)
            response = conn.getresponse()
            if self.command == "MOVE":
                self.server.state.observe(response.status)
                if response.status in (201, 204):
                    # Real upstream headers prove native success. No downstream
                    # status, interim response, headers or body were emitted.
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.server.state.dropped_response()
                    print(json.dumps(self.server.state.proof()), flush=True)
                    return
            require(not 300 <= response.status <= 399, "upstream_redirect_refused")
            data = response.read(config.max_body + 1)
            require(len(data) <= config.max_body, "response_body_limit")
            safe_headers = [(name, value) for name, value in response.getheaders() if name.lower() in RESPONSE_HEADERS]
            require(all(not any(ord(c) < 32 or ord(c) == 127 for c in value)
                        for _, value in safe_headers), "invalid_upstream_header")
            size = str(len(data))
            if self.command == "HEAD":
                size = response.getheader("Content-Length", "0")
                require(re.fullmatch(r"[0-9]{1,20}", size), "invalid_upstream_header")
            self.send_response_only(response.status)
            for name, value in safe_headers:
                self.send_header(name, value)
            self.send_header("Content-Length", size)
            self.send_header("Connection", "close")
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)
        except ProxyError as e:
            self.send_error(403 if str(e) in {"method_not_allowed", "path_not_allowed", "move_outside_fixture",
                                            "destination_outside_fixture", "overwrite_forbidden"} else 502)
        except (OSError, http.client.HTTPException, ValueError, IndexError):
            # No raw errors or upstream bodies enter diagnostic output.
            try:
                self.send_error(502)
            except OSError:
                pass
        finally:
            if conn is not None:
                conn.close()
            if self.upstream_socket is not None:
                self.upstream_socket.close()
                self.upstream_socket = None

    do_GET = do_HEAD = do_PROPFIND = do_MOVE = forward
    do_DELETE = do_POST = do_PUT = do_PATCH = do_MKCOL = do_COPY = do_CONNECT = do_OPTIONS = forward


class SafeParser(argparse.ArgumentParser):
    def error(self, message):
        raise ProxyError("invalid_arguments")


def main(argv=None, env=None):
    parser = SafeParser(description=__doc__)
    parser.add_argument("--check-only", action="store_true", help="Validate locally, never bind/connect; exits 3 (not_run).")
    parser.add_argument("--arm", action="store_true", help="Start a one-shot exact-MOVE proxy on loopback.")
    try:
        args = parser.parse_args(argv)
        config = Config.from_environment(os.environ if env is None else env)
        if args.check_only:
            print(json.dumps({"status": "not_run", "execution": "not_run", "reason": "configuration_valid_no_network"}))
            return 3
        require(args.arm, "explicit_arm_required")
        with ProxyServer(config) as server:
            print(json.dumps({"status": "armed", "execution": "not_run", "acceptance": "not_run"}), flush=True)
            try:
                server.serve_forever(poll_interval=0.5)
            except KeyboardInterrupt:
                pass
            finally:
                print(json.dumps(server.state.proof()), flush=True)
        return 3  # A fault helper cannot establish product acceptance.
    except ProxyError as e:
        print(json.dumps({"status": "not_run", "execution": "not_run", "reason": str(e)}))
        return 2
    except (OSError, ssl.SSLError):
        print(json.dumps({"status": "not_run", "execution": "not_run", "reason": "proxy_startup_failed"}))
        return 2


if __name__ == "__main__":
    sys.exit(main())
