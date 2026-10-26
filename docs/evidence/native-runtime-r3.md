# Native R3 runtime restoration and replay

Restored and checked on 2026-10-23, approximately 13:38–13:41 UTC; sqlc restored at approximately 13:49 UTC. Platform: Linux amd64, Debian 13. This document records disposable test tooling, not a production installation or full application acceptance.

Runtime binaries, archives, generated credentials, databases, logs, and build caches belong outside the source repository. Do not include them in a source ZIP. No system packages, system security settings, application dependency versions, or existing release ZIP were changed by this restoration.

## Versions and provenance

| Component | Version | Verification |
|---|---|---|
| Go | 1.25.3 | SHA256 from the official go.dev release JSON |
| PostgreSQL server/client/libpq | 17.11-0+deb13u1 | Debian InRelease signatures, authenticated package-index SHA256, then package SHA256 |
| NATS Server | 2.15.1 | SHA256 in official nats-io/nats-server GitHub release asset metadata |
| Meilisearch | 1.55.0 | SHA256 in official meilisearch/meilisearch GitHub release asset metadata |
| sqlc | 1.29.0 | Official release over HTTPS; locally recorded SHA256 only |

**sqlc limitation:** the v1.29.0 release has no checksum asset, and its GitHub asset `digest` field is null. The hash below is the observed download hash, not a separately authenticated upstream checksum. It must not be described as an upstream-checksum verification.

The official metadata sources used were:

- https://go.dev/dl/?mode=json&include=all
- https://api.github.com/repos/nats-io/nats-server/releases/tags/v2.15.1
- https://api.github.com/repos/meilisearch/meilisearch/releases/tags/v1.55.0
- https://api.github.com/repos/sqlc-dev/sqlc/releases/tags/v1.29.0
- https://deb.debian.org/debian/dists/trixie/InRelease
- https://deb.debian.org/debian/dists/trixie/main/binary-amd64/Packages.xz

Debian verification used `sqv --keyring /usr/share/keyrings/debian-archive-keyring.pgp --cleartext --output authenticated-release InRelease`. The verified signing fingerprints were:

```text
04B54C3CDCA79751B16BC6B5225629DF75B188BD
41587F7DB8C774BCCF131416762F67A0B2C39DE4
B8B80B5B623EAB6AD8775C45B7C5D7D6350947F8
```

The authenticated Release SHA256 for `main/binary-amd64/Packages.xz` was checked before parsing that index. Only the exact `postgresql-17`, `postgresql-client-17`, and `libpq5` 17.11 records were selected. Their downloaded `.deb` SHA256 values were checked before extraction. This was extraction with `dpkg-deb -x`, not package installation.

### Exact archive/binary provenance

- nats-server-v2.15.1-linux-amd64.tar.gz
  - Source: https://github.com/nats-io/nats-server/releases/download/v2.15.1/nats-server-v2.15.1-linux-amd64.tar.gz
  - SHA256: `bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70`
  - Upstream checksum verified: yes.
- go1.25.3.linux-amd64.tar.gz
  - Source: https://go.dev/dl/go1.25.3.linux-amd64.tar.gz
  - SHA256: `0335f314b6e7bfe08c3d0cfaa7c19db961b7b99fb20be62b0a826c992ad14e0f`
  - Upstream checksum verified: yes.
- meilisearch-linux-amd64
  - Source: https://github.com/meilisearch/meilisearch/releases/download/v1.55.0/meilisearch-linux-amd64
  - SHA256: `7e33f5889235f14290074a19a6d3574bd9b10fb2ee8731eabb7dafd0714b0513`
  - Upstream checksum verified: yes.
- postgresql-17_17.11-0+deb13u1_amd64.deb
  - Source: https://deb.debian.org/debian/pool/main/p/postgresql-17/postgresql-17_17.11-0+deb13u1_amd64.deb
  - SHA256: `d2ce1ddffafa783f9acda4c92c86fc21e8288bff3782d739294f14fa797d7886`
  - Upstream checksum verified: yes.
- postgresql-client-17_17.11-0+deb13u1_amd64.deb
  - Source: https://deb.debian.org/debian/pool/main/p/postgresql-17/postgresql-client-17_17.11-0+deb13u1_amd64.deb
  - SHA256: `9d8558f8dd57c8e92e218a20698383575d53742ca3f9e7c2b7fe5f246d5216ae`
  - Upstream checksum verified: yes.
- libpq5_17.11-0+deb13u1_amd64.deb
  - Source: https://deb.debian.org/debian/pool/main/p/postgresql-17/libpq5_17.11-0+deb13u1_amd64.deb
  - SHA256: `20a4c9ef58b4baf90deda67cfb2cc062871c062830dddc234d28f5ac7931b86b`
  - Upstream checksum verified: yes.
- sqlc_1.29.0_linux_amd64.tar.gz
  - Source: https://github.com/sqlc-dev/sqlc/releases/download/v1.29.0/sqlc_1.29.0_linux_amd64.tar.gz
  - SHA256: `4bb811cb031bb04ed45e75e574346fdc5205e6bf2866a60a5da776b4eb306371`
  - Upstream checksum verified: no; local reproducibility hash only.

## Restore outside the repository

Prerequisites: Bash, curl with normal certificate verification, Python 3.12+, tar, `dpkg-deb`, an existing Debian 13-compatible runtime loader/libraries, and a C toolchain if running Go race tests. Use only the exact official artifacts below. If a pinned artifact is unavailable or a hash differs, stop and investigate rather than substituting another project dependency version.

From the repository root, use its parent as the external workspace root:

```sh
set -euo pipefail
export ROOT="$(cd .. && pwd)"
export RUNTIME="$ROOT/dependency-runtime"
export GOPATH="$ROOT/go-path"
export GOCACHE="$ROOT/go-cache"
export GOTOOLCHAIN=local
mkdir -p "$RUNTIME/downloads" "$RUNTIME/bin" "$RUNTIME/postgres" "$ROOT/toolchain" "$GOPATH" "$GOCACHE"

fetch_checked() {
  local url="$1" sha="$2" filename="$3"
  local dst="$RUNTIME/downloads/$filename"
  if [[ ! -f "$dst" ]]; then
    curl --fail --location "$url" --output "$dst.partial"
    mv "$dst.partial" "$dst"
  fi
  printf "%s  %s\n" "$sha" "$dst" | sha256sum --check --status
}
fetch_checked 'https://github.com/nats-io/nats-server/releases/download/v2.15.1/nats-server-v2.15.1-linux-amd64.tar.gz' 'bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70' 'nats-server-v2.15.1-linux-amd64.tar.gz'
fetch_checked 'https://go.dev/dl/go1.25.3.linux-amd64.tar.gz' '0335f314b6e7bfe08c3d0cfaa7c19db961b7b99fb20be62b0a826c992ad14e0f' 'go1.25.3.linux-amd64.tar.gz'
fetch_checked 'https://github.com/meilisearch/meilisearch/releases/download/v1.55.0/meilisearch-linux-amd64' '7e33f5889235f14290074a19a6d3574bd9b10fb2ee8731eabb7dafd0714b0513' 'meilisearch-linux-amd64'
fetch_checked 'https://deb.debian.org/debian/pool/main/p/postgresql-17/postgresql-17_17.11-0+deb13u1_amd64.deb' 'd2ce1ddffafa783f9acda4c92c86fc21e8288bff3782d739294f14fa797d7886' 'postgresql-17_17.11-0+deb13u1_amd64.deb'
fetch_checked 'https://deb.debian.org/debian/pool/main/p/postgresql-17/postgresql-client-17_17.11-0+deb13u1_amd64.deb' '9d8558f8dd57c8e92e218a20698383575d53742ca3f9e7c2b7fe5f246d5216ae' 'postgresql-client-17_17.11-0+deb13u1_amd64.deb'
fetch_checked 'https://deb.debian.org/debian/pool/main/p/postgresql-17/libpq5_17.11-0+deb13u1_amd64.deb' '20a4c9ef58b4baf90deda67cfb2cc062871c062830dddc234d28f5ac7931b86b' 'libpq5_17.11-0+deb13u1_amd64.deb'
fetch_checked 'https://github.com/sqlc-dev/sqlc/releases/download/v1.29.0/sqlc_1.29.0_linux_amd64.tar.gz' '4bb811cb031bb04ed45e75e574346fdc5205e6bf2866a60a5da776b4eb306371' 'sqlc_1.29.0_linux_amd64.tar.gz'

tar -xzf "$RUNTIME/downloads/go1.25.3.linux-amd64.tar.gz" -C "$ROOT/toolchain"
tar -xzf "$RUNTIME/downloads/nats-server-v2.15.1-linux-amd64.tar.gz" -C "$RUNTIME"
tar -xzf "$RUNTIME/downloads/sqlc_1.29.0_linux_amd64.tar.gz" -C "$RUNTIME/bin"
for package in postgresql-17 postgresql-client-17 libpq5; do
  dpkg-deb -x "$RUNTIME/downloads/${package}_17.11-0+deb13u1_amd64.deb" "$RUNTIME/postgres"
done
chmod +x "$RUNTIME/downloads/meilisearch-linux-amd64" "$RUNTIME/bin/sqlc"
# Hard link avoids retaining a duplicate ~341 MiB Meilisearch binary.
if [[ ! -e "$RUNTIME/meilisearch" ]]; then
  ln "$RUNTIME/downloads/meilisearch-linux-amd64" "$RUNTIME/meilisearch"
fi
export LD_LIBRARY_PATH="$RUNTIME/postgres/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
"$ROOT/toolchain/go/bin/go" version
"$RUNTIME/postgres/usr/lib/postgresql/17/bin/postgres" --version
"$RUNTIME/nats-server-v2.15.1-linux-amd64/nats-server" -v
"$RUNTIME/meilisearch" --version
"$RUNTIME/bin/sqlc" version
```

Expected version output is Go 1.25.3, PostgreSQL 17.11, NATS 2.15.1, Meilisearch 1.55.0, and sqlc v1.29.0. sqlc was obtained as an official prebuilt release, so this step does not populate another Go module/build cache. If rebuilding through the Go registry instead, retain the same `GOPATH` and `GOCACHE` and pin `github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0`.

## Recreate the disposable same-exec runner

All services and the consumers that test them must be launched from one shell/tool exec invocation. Do not start a daemon in one tool invocation and assume another invocation has the same network namespace. The following is the exact runner source, provided here so the source package does not depend on packaging runtime binaries or a preexisting external helper.

Save it as `$RUNTIME/with-native-deps.sh` and mark it executable:

```sh
cat > "$RUNTIME/with-native-deps.sh" <<'NATIVE_RUNNER'
#!/usr/bin/env bash
# Execute services AND the supplied test/application command in one process/network
# namespace. Synthetic, loopback-only disposable services; no system installation.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE="$(dirname "$ROOT")"
PG_BIN="$ROOT/postgres/usr/lib/postgresql/17/bin"
export LD_LIBRARY_PATH="$ROOT/postgres/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export PATH="$WORKSPACE/toolchain/go/bin:$ROOT/bin:$PG_BIN:$PATH"
export GO_BIN="$WORKSPACE/toolchain/go/bin/go"
export PSQL="$PG_BIN/psql"
export GOTOOLCHAIN=local
export GOPATH="${GOPATH:-$WORKSPACE/go-path}"
export GOCACHE="${GOCACHE:-$WORKSPACE/go-cache}"
export SQLC_BIN="$ROOT/bin/sqlc"
[[ $# -gt 0 ]] || { echo "Usage: $0 command [args ...]" >&2; exit 2; }
# Each call owns a new bounded scratch directory. Retain logs but remove data by
# default after graceful shutdown. KEEP_NATIVE_DATA=1 retains this synthetic data.
mkdir -p "$ROOT/runs"
RUN="$(mktemp -d "$ROOT/runs/run.XXXXXXXX")"
export NATIVE_RUN_DIR="$RUN"
PG_PORT="${NATIVE_PG_PORT:-55432}"
NATS_PORT="${NATIVE_NATS_PORT:-54222}"
NATS_HTTP_PORT="${NATIVE_NATS_HTTP_PORT:-58222}"
MEILI_PORT="${NATIVE_MEILI_PORT:-57700}"
NATS_PID=; MEILI_PID=; PG_STARTED=0
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$MEILI_PID" ]]; then kill -TERM "$MEILI_PID" 2>/dev/null || true; fi
  if [[ -n "$NATS_PID" ]]; then kill -TERM "$NATS_PID" 2>/dev/null || true; fi
  if [[ "$PG_STARTED" == 1 ]]; then "$PG_BIN/pg_ctl" -D "$RUN/pgdata" -m fast -w -t 30 stop >>"$RUN/postgres.log" 2>&1 || status=1; fi
  if [[ -n "$MEILI_PID" ]]; then wait "$MEILI_PID" 2>/dev/null || true; fi
  if [[ -n "$NATS_PID" ]]; then wait "$NATS_PID" 2>/dev/null || true; fi
  if [[ "${KEEP_NATIVE_DATA:-0}" != 1 ]]; then rm -rf -- "$RUN/pgdata" "$RUN/jetstream" "$RUN/meili-data"; fi
  printf 'Native runtime logs: %s\n' "$RUN"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# UTF8 C locale is deterministic; trust is restricted to isolated loopback.
"$PG_BIN/initdb" -D "$RUN/pgdata" -L "$ROOT/postgres/usr/share/postgresql/17" --username=drive --auth=trust --no-locale --encoding=UTF8 >"$RUN/initdb.log" 2>&1
cat >>"$RUN/pgdata/postgresql.conf" <<EOF
listen_addresses = '127.0.0.1'
port = $PG_PORT
unix_socket_directories = ''
shared_buffers = '64MB'
work_mem = '4MB'
max_connections = 60
max_wal_size = '128MB'
min_wal_size = '32MB'
logging_collector = off
EOF
PG_STARTED=1
"$PG_BIN/pg_ctl" -D "$RUN/pgdata" -l "$RUN/postgres.log" -w -t 30 start
"$PG_BIN/createdb" -h 127.0.0.1 -p "$PG_PORT" -U drive clouddrive
if [[ "${NATIVE_SKIP_NATS_MEILI:-0}" != 1 ]]; then
cat >"$RUN/nats.conf" <<EOF
listen: 127.0.0.1:$NATS_PORT
http: 127.0.0.1:$NATS_HTTP_PORT
server_name: disposable-clouddrive-test
jetstream {
  store_dir: "$RUN/jetstream"
  max_mem_store: 256MB
  max_file_store: 1GB
}
debug: false
trace: false
EOF
"$ROOT/nats-server-v2.15.1-linux-amd64/nats-server" -c "$RUN/nats.conf" >"$RUN/nats.log" 2>&1 &
NATS_PID=$!
"$ROOT/meilisearch" --db-path "$RUN/meili-data" --http-addr "127.0.0.1:$MEILI_PORT" --env development --no-analytics --max-indexing-memory 512MiB --max-indexing-threads 2 >"$RUN/meili.log" 2>&1 &
MEILI_PID=$!
fi
export DRIVE_TEST_DATABASE_URL="postgres://drive@127.0.0.1:$PG_PORT/clouddrive?sslmode=disable"
export DRIVE_TEST_NATS_URL="nats://127.0.0.1:$NATS_PORT"
export DRIVE_TEST_MEILI_URL="http://127.0.0.1:$MEILI_PORT"
export DRIVE_TEST_MEILI_KEY=""
# Application services launched by caller inherit the same local endpoints.
export DRIVE_DATABASE_URL="$DRIVE_TEST_DATABASE_URL"
export DRIVE_NATS_URL="$DRIVE_TEST_NATS_URL"
export DRIVE_MEILI_URL="$DRIVE_TEST_MEILI_URL"
export DRIVE_MEILI_API_KEY=""
unset DRIVE_MEILI_API_KEY_FILE
if [[ "${NATIVE_SKIP_NATS_MEILI:-0}" != 1 ]]; then
python3 - "$NATS_HTTP_PORT" "$MEILI_PORT" <<'PY'
import json, sys, time, urllib.request
urls=[f'http://127.0.0.1:{sys.argv[1]}/healthz',f'http://127.0.0.1:{sys.argv[2]}/health']
for url in urls:
 deadline=time.monotonic()+30
 while True:
  try:
   with urllib.request.urlopen(url,timeout=1) as r:
    body=json.load(r)
    if r.status==200 and body.get('status') in ('ok','available'): break
  except (OSError,ValueError): pass
  if time.monotonic()>deadline: raise SystemExit(f'Native service failed health check: {url}')
  time.sleep(.2)
print('Native dependencies ready: PostgreSQL, NATS JetStream, Meilisearch.')
PY
else
  printf 'PostgreSQL ready; NATS and Meilisearch startup delegated to caller.\n'
fi
"$@"
NATIVE_RUNNER
chmod +x "$RUNTIME/with-native-deps.sh"
```

The runner starts loopback-only PostgreSQL with `unix_socket_directories=''`, uses disposable trust authentication only for this isolated synthetic test database, and preserves command exit status. It removes its owned generated service data after graceful shutdown while retaining logs. `KEEP_NATIVE_DATA=1` retains synthetic state for diagnosis. Never point it at existing user or production data.

Default ports: PostgreSQL 55432, NATS 54222, NATS health 58222, Meilisearch 57700. Override with `NATIVE_PG_PORT`, `NATIVE_NATS_PORT`, `NATIVE_NATS_HTTP_PORT`, and `NATIVE_MEILI_PORT`. NATS test limits are 256MB memory and 1GB files. Meilisearch indexing is limited to 512MiB and 2 threads with telemetry disabled. PostgreSQL uses 64MB shared buffers and 128MB maximum WAL. These are process-local test configurations, not host changes. Keep synthetic payloads bounded; these settings alone are not an aggregate filesystem quota.

### Run native SQL/application tests

From the repository root:

```sh
"$RUNTIME/with-native-deps.sh" bash -c 'DATABASE_URL="$DRIVE_TEST_DATABASE_URL" PSQL="$PSQL" bash db/tests/run.sh'
"$RUNTIME/with-native-deps.sh" bash -c '"$GO_BIN" test -race -count=1 -v ./...'
```

The first command was executed successfully during this restoration. The second is the replay command for the broader application tests; this document does not claim that restoration itself ran that full suite.

For a Python supervisor that must hold NATS and Meilisearch Popen objects for stop/restart tests, let the supervisor own those processes and use PostgreSQL-only mode:

```sh
NATIVE_SKIP_NATS_MEILI=1 "$RUNTIME/with-native-deps.sh" python3 /path/to/the/authorized-supervisor.py
```

This mode opens and health-checks only PostgreSQL. It still exports the configured NATS/Meilisearch URLs for the caller. It does not start duplicate NATS/Meilisearch processes. Use a different PostgreSQL port, such as `NATIVE_PG_PORT=55433`, for an independent concurrent regression run.

Actual Keycloak/oCIS restoration and verified-TLS identity/storage smoke are described separately in [native-stack-restoration.md](native-stack-restoration.md). Their helper is `scripts/native_identity_storage.sh`. The application supervisor must start those services and all consumers within the same exec namespace as this runner. Credential files and test CA private keys remain external and must not be copied into evidence or a source ZIP.

### sqlc regeneration

The restored executable is `$RUNTIME/bin/sqlc`. To regenerate after intentionally changing the SQL queries, run from the repository root:

```sh
"$RUNTIME/bin/sqlc" generate
```

For a non-mutating generation-drift check, copy `sqlc.yaml` and `db/` to a disposable directory, run sqlc there, and compare its `internal/dbgen` output against the repository. The existing `scripts/selftest.sh` does this when `SQLC_BIN` points at the restored executable.

## Actual restoration checks and limits

The following checks completed against the real restored binaries:

- All five executable version checks returned the pinned versions.
- Actual PostgreSQL `SELECT version()` succeeded; `SHOW unix_socket_directories` was empty.
- Actual NATS `/healthz` returned `{"status":"ok"}` and Meilisearch `/health` returned `{"status":"available"}`.
- PostgreSQL-only mode returned a successful SQL query while NATS/Meilisearch ports were unoccupied, allowing supervisor ownership.
- A caller exiting with status 23 retained status 23 after cleanup.
- PostgreSQL and NATS shutdown were visible in logs, and the runner removed its owned service data.
- The shipped `db/tests/run.sh` passed against real PostgreSQL 17.11, including quota, unknown-outcome safeguards, idempotency, immutability, version/generation priority, stale workers, share approval, disable/re-enable, anonymous feedback, and ancestor gates.

Workspace-only restoration logs were `dependency-runtime/native-health-smoke.log`, `postgres-only-smoke.log`, `nonzero-cleanup-smoke.log`, and `sql-invariants.log`; source/provenance metadata was `runtime-provenance.json`. These local artifacts are evidence references, not an instruction to include runtime directories in the release. The official source URLs, recorded hashes, recreation procedure, and observed results are preserved in this document.

These checks do not establish full application stress/fault acceptance, complete Compose deployment, real-browser operation, Peri interoperability, S3 integration, or a 10 GiB interrupted/resumed transfer. Those require their own actual execution and evidence. Do not turn absent integrations or prepared replay commands into passes.
