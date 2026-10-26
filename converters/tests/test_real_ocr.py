"""Real CPU inference only. Skipped unless local model directory is provided."""
import base64
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import unittest
import urllib.request
from PIL import Image, ImageDraw, ImageFont

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import adapter


def image():
    out = io.BytesIO()
    canvas = Image.new("RGB", (1500, 400), "white")
    font = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 64)
    draw = ImageDraw.Draw(canvas)
    draw.text((60, 60), "CloudDrive OCR test 12345", font=font, fill="black")
    draw.text((60, 180), "Stable source version 2026", font=font, fill="black")
    canvas.save(out, "PNG")
    return out.getvalue()


@unittest.skipUnless(os.environ.get("RUN_REAL_OCR") == "1" and os.environ.get("PADDLE_MODELS_DIR"), "real PaddleOCR model execution not requested/configured")
class RealOCRTests(unittest.TestCase):
    def test_native_http_inference_and_scanned_pdf(self):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0)); port = sock.getsockname()[1]
        environment = dict(os.environ, ADAPTER_TOKEN="test-real-ocr-only", PORT=str(port), ADAPTER_MODE="paddleocr", ADAPTER_MEMORY_MB="4096", ADAPTER_TIMEOUT_SECONDS="180", OPENBLAS_NUM_THREADS="2", OMP_NUM_THREADS="2")
        process = subprocess.Popen([sys.executable, str(Path(adapter.__file__))], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        base = f"http://127.0.0.1:{port}"
        try:
            for _ in range(100):
                try:
                    urllib.request.urlopen(base + "/healthz", timeout=.2); break
                except OSError:
                    time.sleep(.05)
            data = image()
            request = {"job_id": "real-ocr", "attempt_id": "1", "output_scope": "tmp/real-ocr/1/", "input": {"filename": "scan.png", "content_base64": base64.b64encode(data).decode()}, "recipe": {"timeout_seconds": 180, "max_pages": 1}}
            message = urllib.request.Request(base + "/v1/execute", data=json.dumps(request).encode(), headers={"Authorization": "Bearer test-real-ocr-only", "Content-Type": "application/json"})
            result = json.load(urllib.request.urlopen(message, timeout=190))
            self.assertEqual(result["status"], "ready", result)
            self.assertIn("12345", result["markdown"])
            self.assertIn("CloudDrive", result["markdown"].replace(" ", ""))
            self.assertEqual(result["engine"]["version"], "3.3.2")
            from reportlab.pdfgen import canvas
            from reportlab.lib.utils import ImageReader
            output = io.BytesIO(); pdf = canvas.Canvas(output, pagesize=(750, 200)); pdf.drawImage(ImageReader(io.BytesIO(data)), 0, 0, width=750, height=200); pdf.save()
            request["input"] = {"filename": "scan.pdf", "content_base64": base64.b64encode(output.getvalue()).decode()}
            old = {k: os.environ.get(k) for k in ("DRIVE_OCR_URL", "ADAPTER_TOKEN", "DRIVE_VISION_BASE_URL", "DRIVE_VISION_MODEL")}
            try:
                os.environ.update(DRIVE_OCR_URL=base, ADAPTER_TOKEN="test-real-ocr-only", DRIVE_VISION_BASE_URL="", DRIVE_VISION_MODEL="")
                converted = adapter.execute(request, mode="anydoc")
            finally:
                for key, value in old.items():
                    if value is None: os.environ.pop(key, None)
                    else: os.environ[key] = value
            self.assertEqual(converted["status"], "partial", converted)
            self.assertEqual(converted["completeness"]["missing_text_pages"], [], converted)
            self.assertEqual(converted["completeness"]["missing_description_pages"], [1], converted)
            self.assertIn("12345", converted["markdown"])
            print("REAL CPU OCR text:", result["markdown"])
        finally:
            process.terminate(); process.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main(verbosity=2)
