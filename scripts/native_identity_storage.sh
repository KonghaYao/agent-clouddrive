#!/usr/bin/env bash
# Source this file, call native_identity_prepare, then native_identity_start in the
# SAME execution environment as the application/test process. Synthetic use only.
NATIVE_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NATIVE_RUNTIME="${NATIVE_RUNTIME:-$(dirname "$NATIVE_REPO_ROOT")/dependency-runtime}"

native_identity_prepare() {
  umask 077
  export NATIVE_RUNTIME
  export NATIVE_PUBLIC_URL="${NATIVE_PUBLIC_URL:-http://127.0.0.1:58081}"
  mkdir -p "$NATIVE_RUNTIME"/{bin,downloads,tls,identity,logs,ocis-config,ocis-data}
  if [[ ! -x "$NATIVE_RUNTIME/keycloak-26.8.1/bin/kc.sh" ]]; then
    curl -fsSL --retry 2 https://github.com/keycloak/keycloak/releases/download/26.8.1/keycloak-26.8.1.tar.gz -o "$NATIVE_RUNTIME/downloads/keycloak.tar.gz" || return
    tar -xzf "$NATIVE_RUNTIME/downloads/keycloak.tar.gz" -C "$NATIVE_RUNTIME" || return
  fi
  if [[ ! -x "$NATIVE_RUNTIME/bin/ocis" ]]; then
    curl -fsSL --retry 2 https://github.com/owncloud/ocis/releases/download/v8.2.2/ocis-8.2.2-linux-amd64 -o "$NATIVE_RUNTIME/bin/ocis" || return
    chmod 700 "$NATIVE_RUNTIME/bin/ocis"
  fi
  if [[ ! -f "$NATIVE_RUNTIME/tls/server.crt" ]]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 7 -subj '/CN=CloudDrive disposable native test' -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' -addext 'basicConstraints=critical,CA:TRUE' -keyout "$NATIVE_RUNTIME/tls/server.key" -out "$NATIVE_RUNTIME/tls/server.crt" >/dev/null 2>&1 || return
  fi
  export NATIVE_REPO_ROOT
  python3 "$NATIVE_REPO_ROOT/scripts/native_identity_prepare.py" || return
  source "$NATIVE_RUNTIME/identity/native.env"
}

native_identity_start() {
  source "$NATIVE_RUNTIME/identity/native.env"
  JAVA_OPTS_APPEND="-Xms128m -Xmx768m" "$NATIVE_RUNTIME/keycloak-26.8.1/bin/kc.sh" start-dev --http-host=127.0.0.1 --http-port=58080 --https-port=58443 --https-certificate-file="$TEST_CA_FILE" --https-certificate-key-file="$NATIVE_RUNTIME/tls/server.key" --hostname=https://localhost:58443 --import-realm >"$NATIVE_RUNTIME/logs/keycloak.log" 2>&1 &
  NATIVE_KEYCLOAK_PID=$!; export NATIVE_KEYCLOAK_PID
  local ready=0
  for _ in {1..120}; do
    if curl -fsS --cacert "$TEST_CA_FILE" "$TEST_OIDC_ISSUER/.well-known/openid-configuration" >/dev/null 2>&1; then ready=1; break; fi
    kill -0 "$NATIVE_KEYCLOAK_PID" 2>/dev/null || { echo 'Keycloak exited; inspect local log'; return 1; }
    sleep 1
  done
  [[ $ready == 1 ]] || { echo 'Keycloak startup timed out'; return 1; }
  (
    source "$NATIVE_RUNTIME/identity/ocis.env"
    export GOMEMLIMIT=768MiB GOMAXPROCS=2
    if [[ ! -f "$OCIS_CONFIG_DIR/ocis.yaml" ]]; then
      "$NATIVE_RUNTIME/bin/ocis" init >"$NATIVE_RUNTIME/logs/ocis-init.log" 2>&1 || exit
      sed -i -E 's/(password[[:space:]]*:).*/\1 [redacted]/' "$NATIVE_RUNTIME/logs/ocis-init.log"
    fi
    exec "$NATIVE_RUNTIME/bin/ocis" server
  ) >"$NATIVE_RUNTIME/logs/ocis.log" 2>&1 &
  NATIVE_OCIS_PID=$!; export NATIVE_OCIS_PID
  ready=0
  for _ in {1..120}; do
    if curl -fsS --cacert "$TEST_CA_FILE" "$DRIVE_OCIS_INTERNAL_URL/status.php" >/dev/null 2>&1; then ready=1; break; fi
    kill -0 "$NATIVE_OCIS_PID" 2>/dev/null || { echo 'oCIS exited; inspect local log'; return 1; }
    sleep 1
  done
  [[ $ready == 1 ]] || { echo 'oCIS startup timed out'; return 1; }
  echo 'Native Keycloak and oCIS ready (TLS verified).'
}

native_identity_stop() {
  for pid in "${NATIVE_OCIS_PID:-}" "${NATIVE_KEYCLOAK_PID:-}"; do [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true; done
}
