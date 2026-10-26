#!/bin/sh
set -eu
# Go's config supports most *_FILE variables directly. Database/NATS URLs are
# expanded here without printing them or placing credentials in Compose YAML.
if [ -n "${DRIVE_DATABASE_URL_FILE:-}" ]; then
  DRIVE_DATABASE_URL=$(cat "$DRIVE_DATABASE_URL_FILE"); export DRIVE_DATABASE_URL
fi
if [ -n "${DRIVE_NATS_URL_FILE:-}" ]; then
  DRIVE_NATS_URL=$(cat "$DRIVE_NATS_URL_FILE"); export DRIVE_NATS_URL
fi
exec "$@"
