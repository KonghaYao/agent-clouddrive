import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import unittest
import urllib.request
import uuid

HERE = Path(__file__).resolve().parents[1]


def generated(directory, *args):
    result = subprocess.run([os.sys.executable, str(HERE / "setup.py"), "--directory", str(directory), *args], capture_output=True, text=True)
    if result.returncode:
        raise AssertionError("setup 失败: " + result.stderr)
    return (directory / "generated/nginx.conf").read_text()


class RepairCookieTests(unittest.TestCase):
    def test_default_never_trusts_client_protocol(self):
        source = (HERE / "gateway/nginx.conf").read_text()
        self.assertIn('default "secure";', source)
        self.assertNotIn('"nosecure"', source)
        self.assertIn('geo $identity_trusted_proxy', source)
        self.assertIn('"http:1:https" https;', source)
        self.assertIn('default "";', source)
        self.assertIn('X-Forwarded-Proto $identity_scheme;', source)

    def test_only_explicit_local_generation_allows_http(self):
        with tempfile.TemporaryDirectory() as tmp:
            config = generated(Path(tmp), "--development-http")
            self.assertIn('"http:drive.localhost" "nosecure";', config)
            self.assertIn('"http:drive.localhost" "samesite=lax";', config)
            self.assertNotIn('0.0.0.0/0 1;', config)

    def test_realm_replay_enabled_without_login_activity_capture(self):
        import sys
        sys.path.insert(0, str(HERE))
        from keycloak.realm_template import realm
        value = realm()
        self.assertTrue(value["eventsEnabled"])
        self.assertTrue(value["adminEventsEnabled"])
        self.assertTrue(value["adminEventsDetailsEnabled"])
        self.assertEqual(value["eventsListeners"], [])
        self.assertNotIn("LOGIN", value["enabledEventTypes"])
        self.assertIn("view-events", value["users"][0]["clientRoles"]["realm-management"])

    @unittest.skipUnless(os.environ.get("REPAIR_NGINX_LIVE") == "1", "需要显式运行真实 Docker nginx/TLS")
    def test_real_nginx_tls_proxy_and_forged_headers(self):
        image = os.environ.get("REPAIR_NGINX_IMAGE", "clouddrive-demo-df68102d15f5/gateway:1.0.0")

        class Upstream(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Set-Cookie", "identity=test; Path=/; Secure; HttpOnly; SameSite=None")
                self.send_header("X-Observed-Proto", self.headers.get("X-Forwarded-Proto", ""))
                self.send_header("X-Gateway-Peer", self.headers.get("X-Gateway-Peer", ""))
                self.end_headers()
                self.wfile.write(b"ok")

            def log_message(self, *_):
                pass

        # Docker Desktop 的 host.docker.internal 通过本机监听进入 fixture。
        upstream = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Upstream)
        thread = threading.Thread(target=upstream.serve_forever)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                tmp = Path(tmp)
                cert, key = tmp / "cert.pem", tmp / "key.pem"
                subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(key), "-out", str(cert), "-days", "1", "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
                context = ssl.create_default_context(cafile=str(cert))
                production = generated(tmp / "production", "--public-url", "https://drive.example.test", "--tls-cert", str(cert), "--tls-key", str(key))
                self.assertNotIn('"nosecure"', production)
                self.assertNotIn('"samesite=lax"', production)
                self.assertIn("listen 443 ssl;", production)
                # 同一真实配置分别验证不可信来源与显式可信内部代理。
                peer = None
                for trusted in (False, True):
                    with self.subTest(trusted=trusted):
                        args = ["--development-http"]
                        if trusted:
                            args += ["--trusted-proxy-cidr", peer + ("/128" if ":" in peer else "/32")]
                        conf = generated(tmp / ("trusted" if trusted else "direct"), *args)
                        conf = conf.replace("proxy_set_header Host $http_host;", "proxy_set_header X-Gateway-Peer $remote_addr;\n      proxy_set_header Host $http_host;")
                        conf = conf.replace("    listen 8080;", "    listen 8080;\n    listen 8443 ssl;\n    ssl_certificate /proof/cert.pem;\n    ssl_certificate_key /proof/key.pem;")
                        conf = conf.replace("http://keycloak:8080", "http://host.docker.internal:" + str(upstream.server_port))
                        conf = conf.replace("http://api:8080", "http://host.docker.internal:" + str(upstream.server_port)).replace("http://api:8081", "http://host.docker.internal:" + str(upstream.server_port))
                        config = tmp / "nginx.conf"
                        config.write_text(conf)
                        name = "repair-e-nginx-" + uuid.uuid4().hex
                        subprocess.run(["docker", "run", "-d", "--name", name, "-v", str(tmp) + ":/proof:ro", "-p", "127.0.0.1::8080", "-p", "127.0.0.1::8443", "--entrypoint", "nginx", image, "-c", "/proof/nginx.conf", "-g", "daemon off;"], capture_output=True, check=True)
                        try:
                            ports = json.loads(subprocess.check_output(["docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", name]))
                            http_port = ports["8080/tcp"][0]["HostPort"]
                            tls_port = ports["8443/tcp"][0]["HostPort"]
                            for _ in range(50):
                                try:
                                    with socket.create_connection(("127.0.0.1", int(http_port)), timeout=.2):
                                        break
                                except OSError:
                                    time.sleep(.1)
                            for protocol, port in (("https", tls_port), ("http", http_port)):
                                for spoof in (None, "http", "https"):
                                    with self.subTest(protocol=protocol, spoof=spoof):
                                        headers = {"Host": "drive.localhost"}
                                        if spoof:
                                            headers["X-Forwarded-Proto"] = spoof
                                        req = urllib.request.Request(protocol + "://127.0.0.1:" + port + "/identity/test", headers=headers)
                                        with urllib.request.urlopen(req, context=context, timeout=10) as response:
                                            peer = response.headers["X-Gateway-Peer"]
                                            cookie = response.headers["Set-Cookie"].lower()
                                            effective = "https" if protocol == "https" or trusted and spoof == "https" else "http"
                                            self.assertEqual(response.headers["X-Observed-Proto"], effective)
                                            self.assertIn("httponly", cookie)
                                            if effective == "https":
                                                self.assertIn("; secure", cookie)
                                                self.assertIn("samesite=none", cookie)
                                            else:
                                                self.assertNotIn("; secure", cookie)
                                                self.assertIn("samesite=lax", cookie)
                            print("真实 nginx：TLS 直连、可信代理、伪造协议头、开发 HTTP Cookie 检查通过")
                        finally:
                            subprocess.run(["docker", "rm", "-f", name], capture_output=True, check=True)
        finally:
            upstream.shutdown()
            thread.join()
            upstream.server_close()


if __name__ == "__main__":
    unittest.main()
