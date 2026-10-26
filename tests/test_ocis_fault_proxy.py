"""Proxy-unit checks with explicit fake I/O; NOT real oCIS or A23 acceptance."""
import contextlib
from email.message import Message
import importlib.util
import io
import json
from pathlib import Path
import ssl
import sys
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

MODULE = Path(__file__).resolve().parent / "integration" / "ocis_response_loss_proxy.py"
spec = importlib.util.spec_from_file_location("ocis_fault_proxy_unit_target", MODULE)
proxy = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = proxy
spec.loader.exec_module(proxy)


def config(**kwargs):
    values = dict(upstream="https://ocis.test.invalid", app_origin="https://fixture-gateway.test.invalid",
                  move_path="/dav/spaces/space!fixture", destination_path="/dav/spaces/space!root/a23-fixture-moved.txt")
    values.update(kwargs)
    return proxy.Config(**values)


def headers(**changes):
    values = {"Host": "fixture-gateway.test.invalid", "Authorization": "Bearer synthetic-proxy-unit-token",
              "Content-Length": "0", "Destination": config().app_origin + config().destination_path,
              "Overwrite": "F", "If-Match": '"synthetic-unit-etag"'}
    values.update({key.replace("_", "-"): value for key, value in changes.items()})
    result = Message()
    for key, value in values.items():
        if value is not None:
            result[key.replace("_", "-")] = value
    return result


def environment(**changes):
    c = config()
    values = {"ALLOW_DISPOSABLE_TEST_FAULTS": "yes", "TEST_OCIS_UPSTREAM_URL": c.upstream,
              "TEST_OCIS_APP_ORIGIN": c.app_origin, "TEST_OCIS_FAULT_MOVE_PATH": c.move_path,
              "TEST_OCIS_FAULT_DESTINATION_PATH": c.destination_path}
    values.update(changes)
    return values


class FakeSocket:
    """No operating-system socket is created."""
    def __init__(self):
        self.shutdowns = []
        self.closed = False

    def shutdown(self, how):
        self.shutdowns.append(how)

    def close(self):
        self.closed = True

    def dup(self):
        return self


class FakeResponse:
    def __init__(self, status=204, body=b"", response_headers=None):
        self.status = status
        self.body = body
        self.headers = response_headers or []
        self.reads = 0

    def read(self, limit):
        self.reads += 1
        return self.body[:limit]

    def getheaders(self):
        return self.headers

    def getheader(self, name, default=None):
        return next((v for k, v in self.headers if k.lower() == name.lower()), default)


class FakeHTTPS:
    def __init__(self, response=None, exception=None):
        self.response = response or FakeResponse()
        self.exception = exception
        self.sock = FakeSocket()
        self.requests = []
        self.connected = False
        self.closed = False

    def connect(self):
        self.connected = True

    def request(self, *args, **kwargs):
        self.requests.append((args, kwargs))

    def getresponse(self):
        if self.exception:
            raise self.exception
        return self.response

    def close(self):
        self.closed = True


def handler(method="MOVE", path=None, request_headers=None, state=None, cfg=None, body=b""):
    """Bypass HTTPServer entirely; only exercise helper control flow in memory."""
    h = object.__new__(proxy.Handler)
    c = cfg or config()
    h.server = SimpleNamespace(config=c, state=state or proxy.State())
    h.command = method
    h.path = path or c.move_path
    h.raw_requestline = (method + " " + h.path + " HTTP/1.1\r\n").encode()
    h.request_version = "HTTP/1.1"
    h.headers = request_headers or headers()
    h.rfile = io.BytesIO(body)
    h.wfile = io.BytesIO()
    h.connection = FakeSocket()
    h.upstream_socket = None
    h.expired = threading.Event()
    return h


class ProxyUnitConfiguration(unittest.TestCase):
    def test_verified_tls_and_loopback_default(self):
        c = config()
        self.assertEqual(c.bind, "127.0.0.1")
        self.assertEqual(c.tls_context.verify_mode, ssl.CERT_REQUIRED)
        self.assertTrue(c.tls_context.check_hostname)

    def test_rejects_untrusted_or_non_origin_upstreams(self):
        for value in ("http://ocis.test.invalid", "https://name:secret@ocis.test.invalid", "https://@ocis.test.invalid",
                      "https://ocis.test.invalid?token=secret", "https://ocis.test.invalid/path", "https://ocis.test.invalid/#x",
                      "https://ocis.test.invalid:bad", "https://ocis.test.invalid\\evil", "https://ocis.test.invalid\n"):
            with self.subTest(value=value), self.assertRaises(proxy.ProxyError):
                config(upstream=value)

    def test_rejects_public_bind_or_hostname_resolution(self):
        for value in ("0.0.0.0", "::", "192.168.0.1", "localhost", "localhost.evil.invalid"):
            with self.subTest(value=value), self.assertRaises(proxy.ProxyError):
                config(bind=value)

    def test_requires_exact_single_file_fixture_and_named_destination(self):
        for value in ("/dav/spaces/", "/dav/spaces/id/child", "/dav/spaces/id?secret=x", "/dav/spaces/*",
                      "/dav/spaces/..", "/dav/spaces/id%2fother", "/dav/spaces/id%252fother"):
            with self.subTest(value=value), self.assertRaises(proxy.ProxyError):
                config(move_path=value)
        with self.assertRaises(proxy.ProxyError):
            config(destination_path="/dav/spaces/root/production.txt")

    def test_opt_in_and_limits_fail_closed(self):
        for override in ({"ALLOW_DISPOSABLE_TEST_FAULTS": "no"}, {"TEST_OCIS_PROXY_TIMEOUT": "NaN"},
                         {"TEST_OCIS_PROXY_TIMEOUT": "121"}, {"TEST_OCIS_PROXY_MAX_BODY": "999999999"},
                         {"TEST_OCIS_PROXY_PORT": "secret"}, {"TEST_OCIS_PROXY_PORT": "0"}):
            with self.subTest(override=override), self.assertRaises(proxy.ProxyError):
                proxy.Config.from_environment(environment(**override))

    def test_check_only_never_binds_connects_or_reports_pass(self):
        with patch.object(proxy, "ProxyServer") as server, patch.object(proxy.http.client, "HTTPSConnection") as upstream:
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = proxy.main(["--check-only", "--arm"], environment())
            self.assertEqual(code, 3)
            result = json.loads(out.getvalue())
            self.assertEqual(result["status"], "not_run")
            self.assertEqual(result["execution"], "not_run")
            server.assert_not_called()
            upstream.assert_not_called()

    def test_missing_arm_never_binds(self):
        with patch.object(proxy, "ProxyServer") as server, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(proxy.main([], environment()), 2)
            server.assert_not_called()

    def test_errors_never_echo_supplied_secrets(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(proxy.main(["--bad=sensitive-argument"], environment()), 2)
            self.assertEqual(proxy.main(["--check-only"], environment(TEST_OCIS_UPSTREAM_URL="https://user:sensitive-token@host")), 2)
        self.assertNotIn("sensitive", out.getvalue())


class ProxyUnitRequestSafety(unittest.TestCase):
    def test_forwards_authorization_only_to_fixed_origin_and_rewrites_destination(self):
        c = config()
        outgoing, size = proxy.prepare_request(c, "MOVE", c.move_path, headers(Host="attacker.invalid", Proxy_Authorization="secret", Cookie="secret"))
        self.assertEqual(outgoing["authorization"], "Bearer synthetic-proxy-unit-token")
        self.assertEqual(outgoing["Destination"], c.upstream + c.destination_path)
        self.assertEqual(size, 0)
        for forbidden in ("host", "proxy-authorization", "cookie"):
            self.assertNotIn(forbidden, outgoing)

    def test_denies_every_native_destructive_or_unrelated_write(self):
        for method in ("DELETE", "PURGE", "POST", "PUT", "PATCH", "MKCOL", "COPY", "CONNECT"):
            with self.subTest(method=method), self.assertRaises(proxy.ProxyError):
                proxy.prepare_request(config(), method, config().move_path, headers())

    def test_never_matches_neighbor_absolute_target_or_destination(self):
        for path in ("/dav/spaces/space!other", config().move_path + "?x=1", "//attacker.invalid/path",
                     "https://attacker.invalid" + config().move_path, "/dav/spaces/%2e%2e", "/dav/spaces/%XX"):
            with self.subTest(path=path), self.assertRaises(proxy.ProxyError):
                proxy.prepare_request(config(), "MOVE", path, headers())
        for value in ("https://attacker.invalid" + config().destination_path, config().app_origin + config().destination_path + "?x=1"):
            with self.subTest(destination=value), self.assertRaises(proxy.ProxyError):
                proxy.prepare_request(config(), "MOVE", config().move_path, headers(Destination=value))

    def test_rejects_unsafe_preconditions_and_framing(self):
        variants = [headers(Overwrite="T"), headers(If_Match="*"), headers(If_Match='W/"weak"'),
                    headers(Expect="100-continue"), headers(Transfer_Encoding="chunked"),
                    headers(Content_Length="-1"), headers(Content_Length="1"), headers(Authorization="secret\r\nInjected: value"),
                    headers(Connection="Authorization")]
        duplicate = headers(); duplicate["Authorization"] = "second"
        variants.append(duplicate)
        duplicate_length = headers(); duplicate_length["Content-Length"] = "0"
        variants.append(duplicate_length)
        for value in variants:
            with self.subTest(value=list(value.keys())), self.assertRaises(proxy.ProxyError):
                proxy.prepare_request(config(), "MOVE", config().move_path, value)

    def test_bounded_propfind_body_and_read_paths(self):
        c = config(max_body=8)
        outgoing, length = proxy.prepare_request(c, "PROPFIND", c.move_path, headers(Destination=None, Content_Length="8"))
        self.assertEqual(length, 8)
        with self.assertRaises(proxy.ProxyError):
            proxy.prepare_request(c, "PROPFIND", c.move_path, headers(Destination=None, Content_Length="9"))
        with self.assertRaises(proxy.ProxyError):
            proxy.prepare_request(c, "GET", "/admin/purge", headers(Destination=None))


class ProxyUnitFakeTransport(unittest.TestCase):
    def invoke(self, h, transport):
        out = io.StringIO()
        with patch.object(proxy.http.client, "HTTPSConnection", return_value=transport) as factory, \
                patch.object(proxy, "deadline_socket", return_value=transport.sock), contextlib.redirect_stdout(out):
            h.forward()
        return out.getvalue(), factory

    def test_success_is_dropped_before_any_downstream_bytes(self):
        for status in (201, 204):
            with self.subTest(status=status):
                h = handler()
                transport = FakeHTTPS(FakeResponse(status, b"sensitive-body"))
                stdout, factory = self.invoke(h, transport)
                self.assertEqual(h.wfile.getvalue(), b"")
                self.assertEqual(len(h.connection.shutdowns), 1)
                self.assertEqual(transport.response.reads, 0)
                factory.assert_called_once_with("ocis.test.invalid", 443, timeout=20, context=h.server.config.tls_context)
                proof = json.loads(stdout)
                self.assertEqual(proof, {"method": "MOVE", "status": status, "counts": {"matching_attempts": 1, "forwarded": 1,
                                                                                         "upstream_success": 1, "responses_dropped": 1}})
                self.assertNotIn("sensitive", stdout)
                self.assertNotIn("synthetic", stdout)
                self.assertTrue(transport.closed)

    def test_second_attempt_never_replays_native_move(self):
        state = proxy.State()
        first = handler(state=state)
        self.invoke(first, FakeHTTPS())
        second = handler(state=state)
        stdout, factory = self.invoke(second, FakeHTTPS())
        factory.assert_not_called()
        self.assertTrue(second.wfile.getvalue().startswith(b"HTTP/1.1 409"))
        self.assertEqual(state.proof()["counts"]["matching_attempts"], 2)
        self.assertEqual(state.proof()["counts"]["forwarded"], 1)

    def test_upstream_failure_is_not_fault_success_or_auto_retry(self):
        h = handler()
        transport = FakeHTTPS(FakeResponse(412))
        stdout, _ = self.invoke(h, transport)
        self.assertEqual(stdout, "")
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 412"))
        self.assertEqual(h.server.state.proof()["counts"]["responses_dropped"], 0)
        self.assertEqual(len(transport.requests), 1)

    def test_transport_exception_is_sanitized_and_arm_is_consumed(self):
        h = handler()
        stdout, _ = self.invoke(h, FakeHTTPS(exception=OSError("https://secret-url.invalid/?token=secret")))
        self.assertEqual(stdout, "")
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 502"))
        self.assertNotIn(b"secret", h.wfile.getvalue())
        self.assertFalse(h.server.state.reserve_move())
        self.assertEqual(h.server.state.proof()["counts"]["upstream_success"], 0)

    def test_upstream_redirect_is_refused_not_followed_or_forwarded(self):
        h = handler(method="GET", request_headers=headers(Destination=None))
        transport = FakeHTTPS(FakeResponse(302, response_headers=[("Location", "https://attacker.invalid/?secret=x")]))
        stdout, factory = self.invoke(h, transport)
        self.assertEqual(stdout, "")
        factory.assert_called_once()
        self.assertEqual(len(transport.requests), 1)
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 502"))
        self.assertNotIn(b"attacker", h.wfile.getvalue())

    def test_oversize_response_is_rejected_before_body_forwarded(self):
        h = handler(method="GET", cfg=config(max_body=4), request_headers=headers(Destination=None))
        self.invoke(h, FakeHTTPS(FakeResponse(200, b"oversized-sensitive-response")))
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 502"))
        self.assertNotIn(b"oversized", h.wfile.getvalue())

    def test_native_delete_denied_before_network(self):
        h = handler(method="DELETE")
        stdout, factory = self.invoke(h, FakeHTTPS())
        factory.assert_not_called()
        self.assertEqual(stdout, "")
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 403"))

    def test_read_does_not_consume_arm_or_forward_sensitive_headers(self):
        h = handler(method="GET", request_headers=headers(Destination=None))
        response = FakeResponse(200, b"fixture", [("ETag", '"fixture"'), ("Set-Cookie", "secret"), ("Authorization", "secret")])
        self.invoke(h, FakeHTTPS(response))
        self.assertTrue(h.wfile.getvalue().endswith(b"fixture"))
        self.assertNotIn(b"secret", h.wfile.getvalue())
        self.assertEqual(h.server.state.proof()["counts"]["forwarded"], 0)

    def test_concurrent_arm_claim_is_atomic(self):
        state = proxy.State()
        results = []
        threads = [threading.Thread(target=lambda: results.append(state.reserve_move())) for _ in range(20)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(sum(results), 1)
        self.assertEqual(state.proof()["counts"]["matching_attempts"], 20)

    def test_hard_deadline_shuts_both_channels_without_logging(self):
        h = handler()
        h.upstream_socket = FakeSocket()
        h.expire()
        self.assertTrue(h.expired.is_set())
        self.assertEqual(len(h.connection.shutdowns), 1)
        self.assertEqual(len(h.upstream_socket.shutdowns), 1)
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            h.log_message("%s", "secret-url")
            proxy.ProxyServer.handle_error(None, None, "secret-address")
        self.assertEqual(out.getvalue() + err.getvalue(), "")

    def test_deadline_already_expired_never_sends_upstream_request(self):
        h = handler()
        h.expired.set()
        transport = FakeHTTPS()
        self.invoke(h, transport)
        self.assertEqual(transport.requests, [])
        self.assertEqual(h.server.state.proof()["counts"]["responses_dropped"], 0)

    def test_tls_deadline_uses_fd_for_shutdown_only_not_ssl_dup(self):
        tls_socket = SimpleNamespace(fileno=lambda: 123)
        fake_socket = FakeSocket()
        with patch.object(proxy.os, "dup", return_value=456) as duplicate, \
                patch.object(proxy.socket, "socket", return_value=fake_socket) as create:
            self.assertIs(proxy.deadline_socket(tls_socket), fake_socket)
            duplicate.assert_called_once_with(123)
            create.assert_called_once_with(fileno=456)

    def test_expect_is_rejected_without_interim_continue(self):
        h = handler()
        self.assertFalse(h.handle_expect_100())
        self.assertTrue(h.wfile.getvalue().startswith(b"HTTP/1.1 417"))
        self.assertNotIn(b"100 Continue", h.wfile.getvalue())


if __name__ == "__main__":
    unittest.main()
