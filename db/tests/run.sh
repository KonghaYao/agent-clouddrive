#!/usr/bin/env bash
set -euo pipefail
: "${DATABASE_URL:?Set DATABASE_URL to a development PostgreSQL database}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PSQL="${PSQL:-psql}"
SCHEMA="drive_db_test_${$}_${RANDOM}"
"$PSQL" "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "CREATE SCHEMA $SCHEMA;" >/dev/null
cleanup() { "$PSQL" "$DATABASE_URL" -v ON_ERROR_STOP=1 -c "DROP SCHEMA IF EXISTS $SCHEMA CASCADE;" >/dev/null 2>&1 || true; }
trap cleanup EXIT
export PGOPTIONS="${PGOPTIONS:-} -c search_path=$SCHEMA,public"
for migration in "$ROOT"/db/migrations/*.up.sql; do
  "$PSQL" "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f "$migration" >/dev/null
done
"$PSQL" "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$ROOT/db/tests/invariants.sql"
