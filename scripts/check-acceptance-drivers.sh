#!/usr/bin/env bash
# OFFLINE driver preparation checks. This script NEVER starts Chromium/services
# and never executes a deployed API scenario or the 10 GiB transfer.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
PYTHON="${PYTHON_BIN:-python3}"
WEB="${DRIVERS_WEB_ROOT:-$ROOT/web}"
LOG="${ACCEPTANCE_DRIVER_LOG:-$ROOT/evidence/acceptance-driver-preparation.log}"
mkdir -p "$(dirname "$LOG")"
exec > >(tee "$LOG") 2>&1
printf 'Scope: OFFLINE acceptance-driver syntax, unit guards, and source contracts ONLY.\n'
printf 'By this offline runner: real browser / deployed API / fault / 10 GiB execution NOT RUN.\n'
date -u '+Started: %Y-%m-%dT%H:%M:%SZ'

for script in "$WEB"/scripts/*acceptance*.mjs "$WEB"/scripts/large-upload-*.mjs; do
  node --check "$script"
done
"$PYTHON" -m py_compile scripts/acceptance.py tests/integration/*.py
"$PYTHON" - <<'PY'
import unittest
root = unittest.defaultTestLoader.discover('tests', pattern='test_acceptance_runner.py')
fault = unittest.defaultTestLoader.discover('tests', pattern='test_ocis_fault_proxy.py')
# Separate discovery is necessary: integration is not an importable package.
live_helpers = unittest.TestLoader().discover('tests/integration', pattern='test_*.py')
for label, suite in [('runner', root), ('fault helper', fault), ('integration driver guards', live_helpers)]:
    if suite.countTestCases() == 0:
        raise SystemExit(f'FAILED: zero {label} driver unit tests discovered')
combined = unittest.TestSuite([root, fault, live_helpers])
if not unittest.TextTestRunner(verbosity=2).run(combined).wasSuccessful():
    raise SystemExit(1)
PY
(cd "$WEB" && node --test scripts/browser-acceptance-unit.mjs scripts/large-upload-unit.mjs)
printf '\nPASSED: requested OFFLINE acceptance-driver preparation checks.\n'
printf 'This offline runner did not execute or promote product acceptance cases.\n'
