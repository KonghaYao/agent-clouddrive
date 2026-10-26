"""Optional real native RustFS check. Never substitutes an in-memory S3 fake."""
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch
import urllib.request

import init_buckets


@unittest.skipUnless(os.environ.get("RUSTFS_BIN"), "native official RustFS binary not configured")
class RustFSBucketTests(unittest.TestCase):
    def test_actual_signed_bucket_creation_is_idempotent(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0)); port = sock.getsockname()[1]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); (root / "data").mkdir()
            key, secret = secrets.token_hex(16), secrets.token_hex(32)
            (root / "key").write_text(key); (root / "secret").write_text(secret)
            environment = dict(os.environ, RUSTFS_ACCESS_KEY=key, RUSTFS_SECRET_KEY=secret,
                               RUSTFS_ADDRESS=f"127.0.0.1:{port}", RUSTFS_CONSOLE_ENABLE="false",
                               RUSTFS_OBS_LOGGER_LEVEL="error", RUSTFS_OBS_LOG_DIRECTORY=str(root / "logs"))
            process = subprocess.Popen([os.environ["RUSTFS_BIN"], str(root / "data")], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            endpoint = f"http://127.0.0.1:{port}"
            try:
                for _ in range(200):
                    if process.poll() is not None:
                        self.fail("RustFS terminated before readiness")
                    try:
                        urllib.request.urlopen(endpoint + "/health", timeout=.2); break
                    except OSError:
                        time.sleep(.1)
                else:
                    self.fail("RustFS did not become ready")
                with patch.dict(os.environ, {"DRIVE_S3_ENDPOINT": endpoint, "DRIVE_S3_REGION": "us-east-1", "DRIVE_S3_ACCESS_KEY_FILE": str(root / "key"), "DRIVE_S3_SECRET_KEY_FILE": str(root / "secret")}):
                    for _ in range(2):
                        # Readiness can precede IAM/S3 readiness; exercise the
                        # same bounded retry routine used by the real init job.
                        init_buckets.initialize(max_attempts=100, retry_seconds=.1)
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill(); process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main(verbosity=2)
