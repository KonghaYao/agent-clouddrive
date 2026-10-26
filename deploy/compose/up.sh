#!/bin/sh
set -eu
cd "$(dirname "$0")"
test -f .env || { echo 'Run python3 setup.py --mode s3 --development-http first.' >&2; exit 1; }
command -v docker >/dev/null || { echo 'Docker Engine and Compose v2 are required; no runtime test has run.' >&2; exit 1; }
docker info >/dev/null
docker compose config --quiet
docker compose build
docker compose up -d postgres nats meilisearch rustfs keycloak init-volumes
docker compose run --rm --no-deps maintenance /app/maintenance migrate
docker compose up -d
echo 'Services started. Inspect docker compose ps and execute acceptance tests; startup is not acceptance.'
echo 'Prepare actual OCR models separately: docker compose --profile setup run --rm prepare-models'
