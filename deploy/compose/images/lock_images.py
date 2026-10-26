#!/usr/bin/env python3
"""Read-only registry manifest resolver. Does not pull/run containers.

Writes exact manifest digests and errors. A digest lookup is NOT runtime or
image-content validation. Run before deployment to refresh evidence deliberately.
"""
import argparse
import concurrent.futures
import datetime
import json
from pathlib import Path
import urllib.error
import urllib.request

IMAGES = ["postgres:17.11-alpine", "nats:2.15.1-alpine", "getmeili/meilisearch:v1.55.0", "owncloud/ocis:8.2.2", "rustfs/rustfs:1.0.3", "quay.io/keycloak/keycloak:26.8.1", "keking/kkfileview:5.0.3", "busybox:1.37.0", "python:3.12.12-slim-bookworm", "golang:1.25.0-bookworm", "node:22.14.0-alpine3.21", "nginx:1.28.0-alpine", "alpine:3.21.3", "maven:3.9.11-eclipse-temurin-21", "eclipse-temurin:21.0.9_10-jre-jammy"]
ACCEPT = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"


def resolve(image):
    try:
        image_name, tag = image.rsplit(":", 1)
        if image_name.startswith("quay.io/"):
            host, repository = "quay.io", image_name[len("quay.io/"):]
            token_url = f"https://quay.io/v2/auth?service=quay.io&scope=repository:{repository}:pull"
        else:
            host = "registry-1.docker.io"
            repository = image_name if "/" in image_name else "library/" + image_name
            token_url = f"https://auth.docker.io/token?service=registry.docker.io&scope=repository:{repository}:pull"
        with urllib.request.urlopen(token_url, timeout=30) as response:
            data = json.load(response)
            token = data.get("token", data.get("access_token"))
        request = urllib.request.Request(f"https://{host}/v2/{repository}/manifests/{tag}", method="GET", headers={"Accept": ACCEPT, "Authorization": "Bearer " + token})
        with urllib.request.urlopen(request, timeout=30) as response:
            digest = response.headers.get("Docker-Content-Digest")
            data = json.load(response)
        if not digest:
            raise ValueError("registry returned no content digest")
        return {"image": image, "digest": digest, "pinned": image + "@" + digest, "media_type": data.get("mediaType"), "status": "registry_manifest_resolved", "runtime_tested": False}
    except Exception as error:
        return {"image": image, "status": "lookup_failed", "error_type": type(error).__name__, "runtime_tested": False}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=Path(__file__).parent / "image-lock.json")
    args = parser.parse_args()
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        images = list(pool.map(resolve, IMAGES))
    args.output.write_text(json.dumps({"resolved_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "images": images}, indent=2) + "\n")
    for image in images:
        print(image["image"], image["status"], image.get("digest", ""))
