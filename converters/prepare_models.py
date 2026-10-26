#!/usr/bin/env python3
"""Explicit one-time official PaddleOCR model preparation, never per request.

Run under the paddleocr image. Downloads only the two named upstream models;
records content digests for the conversion recipe / reproducibility evidence.
Re-running leaves an already populated model directory untouched.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil


def verify(root):
    lockfile = Path(__file__).with_name("model-lock.json")
    if not lockfile.is_file():
        raise SystemExit("Missing checked-in model lock; refusing unpinned model execution")
    lock = json.loads(lockfile.read_text())
    for name, expected in lock["files"].items():
        if Path(name).name not in ("inference.json", "inference.pdiparams", "inference.yml", "config.json"):
            continue
        path = root / name
        if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise SystemExit("Official model checksum mismatch: " + name)
    return hashlib.sha256(lockfile.read_bytes()).hexdigest()


def main():
    root = Path(os.getenv("PADDLE_MODELS_DIR", "/models"))
    root.mkdir(parents=True, exist_ok=True)
    manifest = root / "manifest.json"
    if manifest.exists() and (root / "detection").is_dir() and (root / "recognition").is_dir():
        verify(root)
        print("Models already prepared; no files changed.")
        return
    from paddleocr import PaddleOCR
    PaddleOCR(text_detection_model_name="PP-OCRv5_mobile_det", text_recognition_model_name="PP-OCRv5_mobile_rec",
              use_doc_orientation_classify=False, use_doc_unwarping=False, use_textline_orientation=False, device="cpu")
    cache = Path.home() / ".paddlex" / "official_models"
    for name, destination in (("PP-OCRv5_mobile_det", "detection"), ("PP-OCRv5_mobile_rec", "recognition")):
        source = cache / name
        if not source.is_dir():
            raise SystemExit(f"Upstream model directory missing: {name}; no readiness claimed")
        shutil.copytree(source, root / destination, dirs_exist_ok=True)
    hashes = {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
              for p in sorted(root.rglob("*")) if p.is_file() and p != manifest}
    verify(root)
    manifest.write_text(json.dumps({"paddleocr": "3.3.2", "paddlepaddle": "3.2.2", "models": ["PP-OCRv5_mobile_det", "PP-OCRv5_mobile_rec"], "files": hashes}, indent=2))
    print("Prepared official models and recorded per-file SHA256 checksums.")


if __name__ == "__main__":
    main()
