"""Idempotent S3 bucket create using standard SigV4, no third-party dependency."""
import datetime
import hashlib
import hmac
import os
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET


def create(bucket):
    endpoint = os.environ.get("DRIVE_S3_ENDPOINT", "http://rustfs:9000").rstrip("/")
    region = os.environ.get("DRIVE_S3_REGION", "us-east-1")
    key = Path(os.environ.get("DRIVE_S3_ACCESS_KEY_FILE", "/run/secrets/s3_access_key")).read_text().strip()
    secret = Path(os.environ.get("DRIVE_S3_SECRET_KEY_FILE", "/run/secrets/s3_secret_key")).read_text().strip()
    host = urllib.parse.urlsplit(endpoint).netloc
    now = datetime.datetime.now(datetime.timezone.utc)
    stamp, date = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    digest = hashlib.sha256(b"").hexdigest()
    headers = f"host:{host}\nx-amz-content-sha256:{digest}\nx-amz-date:{stamp}\n"
    signed = "host;x-amz-content-sha256;x-amz-date"
    canonical = f"PUT\n/{bucket}\n\n{headers}\n{signed}\n{digest}"
    scope = f"{date}/{region}/s3/aws4_request"
    message = f"AWS4-HMAC-SHA256\n{stamp}\n{scope}\n{hashlib.sha256(canonical.encode()).hexdigest()}"
    signing_key = ("AWS4" + secret).encode()
    for part in (date, region, "s3", "aws4_request"):
        signing_key = hmac.new(signing_key, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(signing_key, message.encode(), hashlib.sha256).hexdigest()
    request = urllib.request.Request(endpoint + "/" + bucket, data=b"", method="PUT", headers={
        "x-amz-date": stamp, "x-amz-content-sha256": digest,
        "Authorization": f"AWS4-HMAC-SHA256 Credential={key}/{scope}, SignedHeaders={signed}, Signature={signature}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            assert response.status in (200, 204)
    except urllib.error.HTTPError as error:
        body = error.read(4096)
        if error.code != 409 or b"BucketAlreadyOwnedByYou" not in body:
            try:
                code = ET.fromstring(body).findtext("Code", "Unknown")
            except ET.ParseError:
                code = "InvalidErrorResponse"
            if not code.isalnum():
                code = "Unknown"
            raise RuntimeError(f"S3 bucket initialization failed: HTTP {error.code} {code}; credentials omitted") from None


def initialize(max_attempts=30, retry_seconds=2):
    for attempt in range(max_attempts):
        try:
            for bucket in ("clouddrive-originals", "clouddrive-artifacts"):
                create(bucket)
            return
        except (OSError, RuntimeError):
            if attempt == max_attempts - 1:
                raise
            time.sleep(retry_seconds)


if __name__ == "__main__":
    try:
        initialize()
        print("Separate originals/artifacts buckets exist; no lifecycle deletion configured.")
    except (OSError, RuntimeError):
        raise SystemExit("S3 initialization unavailable after retry budget") from None
