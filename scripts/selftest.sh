#!/usr/bin/env bash
# Reproducible source checks. Native integrations run only against explicitly
# configured development endpoints; their absence is reported as SKIPPED.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/clouddrive-selftest.XXXXXX")"
LOG_DIR="${SELFTEST_LOG_DIR:-$ROOT/evidence/selftest}"
mkdir -p "$LOG_DIR"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
exec > >(tee "$LOG_DIR/source-checks.log") 2>&1
step() { printf '\n== %s ==\n' "$*"; }
skip() { printf 'SKIPPED: %s\n' "$*"; }
fail() { printf 'FAILED: %s\n' "$*" >&2; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || fail "Required executable missing: $1"; }
GO="${GO_BIN:-go}"
PYTHON="${PYTHON_BIN:-python3}"
require "$GO"; require "$PYTHON"; require npm; require node; require diff
"$GO" version | grep -Eq 'go version go1\.(2[5-9]|[3-9][0-9])\.' || fail 'Go 1.25+ is required; GO_BIN may select the compiler.'
export GOCACHE="${GOCACHE:-$WORK/go-build}"
export GOPATH="${GOPATH:-$WORK/go-path}"
export GOFLAGS="${GOFLAGS:--mod=readonly}"
export CI=1

step 'Integration boundary declaration (endpoint credentials are never printed)'
for entry in 'PostgreSQL:DRIVE_TEST_DATABASE_URL' 'NATS:DRIVE_TEST_NATS_URL' 'Meilisearch:DRIVE_TEST_MEILI_URL' 'S3/RustFS:DRIVE_TEST_S3_URL' 'oCIS:DRIVE_TEST_OCIS_URL'; do
  label="${entry%%:*}"; variable="${entry#*:}"
  if [[ -n "${!variable:-}" ]]; then
    printf 'ENABLED: %s native tests against supplied development endpoint.\n' "$label"
  else
    skip "$label native tests ($variable is unset)"
    [[ "${SELFTEST_REQUIRE_NATIVE:-0}" != 1 ]] || fail "SELFTEST_REQUIRE_NATIVE=1 requires $variable"
  fi
done
skip 'Full Docker Compose deployment, real Keycloak-to-Peri consent, preview engine end-to-end, and 10 GiB interrupted/resumed browser upload are not asserted by this source test runner.'

step 'Go race tests and static analysis'
"$GO" test -race -count=1 -v ./...
"$GO" vet ./...

step 'sqlc v1.29.0 generation drift check (no Git repository required)'
SQLC="${SQLC_BIN:-}"
if [[ -z "$SQLC" ]]; then
  if command -v sqlc >/dev/null 2>&1 && [[ "$(sqlc version)" == v1.29.0 ]]; then
    SQLC="$(command -v sqlc)"
  else
    mkdir -p "$WORK/bin"
    GOBIN="$WORK/bin" "$GO" install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0
    SQLC="$WORK/bin/sqlc"
  fi
fi
[[ "$("$SQLC" version)" == v1.29.0 ]] || fail 'SQLC_BIN must point to sqlc v1.29.0.'
mkdir -p "$WORK/sqlc"
cp sqlc.yaml "$WORK/sqlc/sqlc.yaml"
cp -R db "$WORK/sqlc/db"
(cd "$WORK/sqlc" && "$SQLC" generate)
diff -ru internal/dbgen "$WORK/sqlc/internal/dbgen"

if [[ -n "${DRIVE_TEST_DATABASE_URL:-}" ]]; then
  step 'PostgreSQL invariant SQL suite in a disposable schema'
  PSQL="${PSQL:-psql}"; require "$PSQL"
  DATABASE_URL="$DRIVE_TEST_DATABASE_URL" PSQL="$PSQL" bash db/tests/run.sh
else
  skip 'PostgreSQL migration/function invariant execution; Go tests above also explicitly skip DB-dependent cases.'
fi

step 'Frontend clean dependency installation, unit tests, and production build'
WEB_WORK="$WORK/web"
"$PYTHON" - "$ROOT/web" "$WEB_WORK" <<'PYWEB'
import shutil, sys
shutil.copytree(sys.argv[1], sys.argv[2], ignore=shutil.ignore_patterns('node_modules', 'dist', 'test-results'))
PYWEB
export npm_config_cache="${npm_config_cache:-$WORK/npm-cache}"
(cd "$WEB_WORK" && npm ci --no-audit --no-fund && npm test && npm run build)

step 'Python converter, deployment, and release-readiness tests'
if [[ "${SELFTEST_USE_CURRENT_PYTHON:-0}" == 1 ]]; then
  printf 'Using caller-provided Python environment; caller is responsible for installing the pinned requirements.\n'
  TEST_PYTHON="$PYTHON"
else
  "$PYTHON" -m venv "$WORK/python"
  TEST_PYTHON="$WORK/python/bin/python"
  # These test-only packages construct real Office fixtures. They are deliberately
  # separate from the production adapter dependency lock.
  "$TEST_PYTHON" -m pip install --disable-pip-version-check -r converters/requirements.txt \
    'reportlab==4.4.9' 'PyYAML==6.0.3' 'python-docx==1.2.0' 'openpyxl==3.1.5' 'python-pptx==1.0.2'
  if [[ "${RUN_REAL_OCR:-0}" == 1 ]]; then
    "$TEST_PYTHON" -m pip install --disable-pip-version-check -r converters/requirements-ocr.txt
  fi
fi
"$TEST_PYTHON" -m unittest discover -s converters/tests -p 'test_*.py' -v
"$TEST_PYTHON" -m unittest discover -s deploy/compose -p 'test_*.py' -v
step 'Local example offline guards (not Docker or browser execution)'
"$TEST_PYTHON" -m unittest discover -s examples/local-demo -p 'test_*.py' -v
"$TEST_PYTHON" - <<'PYREADINESS'
import unittest
suite = unittest.defaultTestLoader.discover(start_dir="tests", pattern="test_*.py")
if suite.countTestCases() == 0:
    raise SystemExit("FAILED: release-readiness test discovery found zero tests")
result = unittest.TextTestRunner(verbosity=2).run(suite)
if not result.wasSuccessful():
    raise SystemExit(1)
PYREADINESS
if [[ "${RUN_REAL_OCR:-0}" != 1 || -z "${PADDLE_MODELS_DIR:-}" ]]; then
  skip 'Real PaddleOCR model inference; set RUN_REAL_OCR=1 and PADDLE_MODELS_DIR to run it.'
fi

step 'Offline live-acceptance driver syntax and guard tests (no browser/services launched)'
# Browser-driver source contracts inspect the actual gateway configuration beside
# the isolated web copy. Do not silently drop that contract in clean self-tests.
mkdir -p "$WORK/deploy/compose"
cp deploy/compose/nginx.conf "$WORK/deploy/compose/nginx.conf"
PYTHON_BIN="$TEST_PYTHON" DRIVERS_WEB_ROOT="$WEB_WORK" ACCEPTANCE_DRIVER_LOG="$LOG_DIR/acceptance-driver-preparation.log" bash scripts/check-acceptance-drivers.sh

if [[ "${SELFTEST_BROWSER:-0}" == 1 ]]; then
  step 'Actual Chromium UI/PKCE smoke (browser sandbox remains enabled)'
  (cd "$WEB_WORK" && npm run test:browser)
  if [[ -d "$WEB_WORK/test-results" ]]; then cp -R "$WEB_WORK/test-results" "$LOG_DIR/browser-results"; fi
else
  skip 'Actual Chromium UI/PKCE smoke; set SELFTEST_BROWSER=1 and optionally CHROMIUM_PATH.'
fi
printf '\nPASSED: all requested source checks completed.\n'
printf 'Skipped/native deployment gates above are NOT converted into acceptance passes.\n'
printf 'Log: %s/source-checks.log\n' "$LOG_DIR"
