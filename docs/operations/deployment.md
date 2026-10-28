# Single-node deployment and operations

## Delivery status and prerequisites

The Compose reference is intended for a single Linux Docker Engine host with Compose v2.39.4 or newer, outbound access during image/model preparation, and adequate disk/RAM. A practical starting point is 4 CPU cores, 12–16 GiB RAM and storage sized for retained original history plus derived outputs. This is a conservative setup suggestion, not a capacity/performance claim.

Actual tests distinguish: (1) local/native dependency integration evidence, (2) converter/domain/frontend tests, (3) Compose schema/config validation, and (4) a real Compose runtime. Docker Engine is absent in the implementation workspace. `docker compose config --quiet` was run with the official standalone Compose v2.39.4 CLI for both generated storage modes, but no Docker containers were started here. Native dependency passes do not replace Compose acceptance.

`deploy/compose/images/image-lock.json` records actual read-only registry manifest lookups. Successfully resolved upstream images are pinned by version AND returned digest in Compose/Dockerfiles. This verifies registry existence, not runtime compatibility. kkFileView 5.0.3 application/base image tags were unavailable in those queries, so `Dockerfile.kkfileview` builds the official v5.0.3 source archive with a verified SHA256 instead of pretending a tag exists or downgrading to a known older security release. A documented one-line source patch forces Office spreadsheets to PDF rather than HTML. This is the project image `agent-clouddrive/kkfileview:5.0.3-pdf1`, not an official upstream image. Its Maven build and real native DOCX/XLSX/PPTX raster integration passed; the Docker build/runtime remains untested.

## First local setup

```sh
cd deploy/compose
python3 setup.py --mode s3 --development-http
# Before enabling application access, execute the M0 gates below on an
# isolated test deployment with this exact version/configuration.
./up.sh
```

Open `http://drive.localhost:8080`. `drive.localhost` resolves to loopback in common browsers; if your OS resolver does not implement `.localhost`, add `127.0.0.1 drive.localhost` to your local hosts file. Inside Docker, the same hostname aliases the gateway. The exact same issuer must be reachable by browser, API and oCIS.

The explicit `--development-http` option is only allowed for a `.localhost` origin and binds the gateway to 127.0.0.1. It sets `sslRequired=none` ONLY in the generated local development realm. The checked-in realm template keeps `sslRequired=external`; do not publish development HTTP configuration to an untrusted network.

`setup.py` creates random secrets once, and preserves them on repeated invocation. A second run does not silently rotate credentials or the 32 raw-byte delegation encryption key. Source files never contain real passwords. Secret/generated parent directories are mode 0700; individually mounted secret files are readable by their intended non-root containers. Keep those directories and `.env` out of Git and delivery ZIPs. Do not print generated realm JSON: it contains generated client secrets.

`up.sh` checks the actual Docker daemon, validates config, builds images, starts dependencies, executes `/app/maintenance migrate`, then starts all services. Migrations are idempotent and retained in the app image. A successful startup is not acceptance. Review `docker compose ps`, then execute the documented functional gates.

### Required integration gates, intentionally disabled by default

The generated `.env` contains `DRIVE_OCIS_INTEGRATION_VERIFIED=false` and `DRIVE_DELEGATION_VERIFIED=false`. The application intentionally refuses unverified original-store access and background delegation. Merely running the quickstart does **not** bypass these gates. The included evidence describes its own native test environment, not your Docker deployment.

Before an operator consciously changes both flags to `true` for an isolated acceptance deployment, verify the M0 contracts with its exact official oCIS/Keycloak versions and configuration: stable resource IDs across rename/MOVE; readable immutable revisions after overwrite/trash/restore; conditional write conflict behavior under the application operation gate; PKCE and correct `sub`/audience/scopes; supported user token exchange to oCIS with `openid`; live disabled-account rejection; explicit offline consent, refresh rotation and revocation. Use the probes under `tests/integration/` plus the native adapter tests described in `docs/operations/selftest.md`, with disposable test accounts only. Changing the flags asserts that those probes have passed; it does not perform verification. Re-run them after identity/storage changes. Production enabling remains an operator decision, and the complete Compose path still requires its own runtime acceptance.

The first Keycloak administrator is `bootstrap-admin`. The random password is in the operator-controlled secret file `secrets/keycloak_admin_password`. Create real users through Keycloak, never through a hardcoded fixture password. The reference realm contains only a service account, not real users. Remove/replace the bootstrap administrator under your identity operations policy after initialization.

For real OCR:

```sh
docker compose --profile setup run --rm prepare-models
docker compose restart paddleocr
```

Model preparation is the only model-download job and has network egress; the ordinary OCR/conversion adapters stay on the internal network. Health endpoints alone do not mean models are ready. Their execute response fails closed if models are absent or their hashes differ.

## HTTPS deployment

Provide your existing trusted CA certificate chain/private key and a real hostname:

```sh
python3 setup.py --mode s3 --public-url https://drive.example.com \
  --tls-cert /secure/fullchain.pem --tls-key /secure/privkey.pem
./up.sh
```

The generator produces an HTTPS nginx listener, mounts TLS material as secret files and sets the external OIDC issuer consistently. The gateway is the only host-published service. This reference accepts HTTP port 8080 or HTTPS port 443 only, preventing a custom public port from pointing internal clients at a nonexistent listener. Internal Docker DNS aliases the same hostname. Use a certificate whose chain is trusted by both browsers and containers; do not disable certificate validation. DNS, certificate issuance/renewal and firewall policy are operator responsibilities. Replacing certificate files requires controlled gateway reload/restart. Changing an established issuer or storage mode is explicitly rejected by the setup script because it is a migration, not a harmless environment switch.

## Services and secrets

- gateway: nginx, static built `web/dist`, same-origin REST and identity login routing. `/mcp` is intentionally not proxied and returns 404; MCP beta is internal-only.
- api: Hertz on 8080; official MCP SDK listener on 8081 (beta, internal only). Neither port is host-published.
- worker/maintenance: separate processes sharing PostgreSQL truth, JetStream and artifact storage.
- PostgreSQL 17.11, NATS 2.15.1, Meilisearch 1.55.0: private persistent dependencies.
- official unmodified `owncloud/ocis:8.2.2`: private native storage kernel; no custom oCIS image or direct client route.
- RustFS 1.0.3: private S3 endpoint, separate `clouddrive-originals`/`clouddrive-artifacts` buckets, console not published.
- Keycloak 26.8.1: identity service reachable only through `/identity/` on the gateway.
- AnyDoc/PaddleOCR/preview/kkFileView: private bounded adapters and preview backend.

Converters receive only their shared adapter token and optionally the vision key. They do not mount PostgreSQL/S3/oCIS root credentials. The backend receives the identity credential JSON, converter token and delegation encryption key via files. Database/NATS URLs containing credentials are expanded by the entrypoint without logging. oCIS/RustFS/Meilisearch upstream variables are loaded from mounted files by small entrypoint scripts because their native settings use environment values.

`DRIVE_VISION_PROVIDER_ID` is a non-secret stable operator-chosen provider identity used with `DRIVE_VISION_MODEL` in the recipe fingerprint. Its default is `unconfigured`. Do not put a key or signed URL in it. A vision provider change must create a distinct recipe; the configured endpoint/key are not accepted from end-user requests.

The web image uses `npm ci` and the committed lockfile. `/config.json` is generated from the selected issuer/client/audience, has `Cache-Control: no-store`, and contains no secrets. `/auth/callback` uses the SPA fallback. `/v1/delegation/callback` is a backend callback, not a SPA route.

## Keycloak clients and background delegation

- `clouddrive-web`: public code flow, S256 PKCE, explicit consent, exact browser callback. Business operation scopes are optional and selected by the web authorization UI.
- `peri`: public code flow + S256 PKCE. Redirect URIs are intentionally empty until the actual Peri client/version/callback is verified. Do not use a wildcard callback to fake compatibility.
- `agent-clouddrive`: confidential token-exchange-only requester, no browser flow, no consent requirement on exchange. Public client tokens carry this audience and the exact MCP resource audience. The exchanged token targets independent `ocis` audience.
- `clouddrive-background`: separate confidential code flow + PKCE + explicit offline consent. Optional `offline_access` is not enabled for the public web/Peri clients. The backend stores refresh material encrypted by `DRIVE_DELEGATION_KEY_FILE`, checks live account state and uses supported token exchange. No administrator impersonation is enabled.
- `clouddrive-identity`: service account with only `realm-management` view-users/query-users, used for authoritative status/group reads, not content impersonation.

The offline scope explicitly maps the `offline_access` role, and a composite default role contains it for normal user creation. Imported users may require explicit `realmRoles: ["default-roles-clouddrive"]` or `offline_access`; do not assume an import grants defaults. Runtime evidence must separately verify normal onboarding inheritance. The reference import is initialization only: Keycloak does not overwrite an existing realm when the same import file changes. Apply reviewed client/role migrations for existing deployments.

The oCIS token exchange must include scope `openid` because oCIS calls the issuer's userinfo endpoint. Keep the public business audience distinct from oCIS. These are native integration gates, not capabilities to assume for every alternative OIDC provider.

## Storage mode, retention and quota

Choose `--mode local` or `--mode s3` before creating real data. Setup maps the project variable `DRIVE_STORAGE_MODE` to the documented official oCIS `STORAGE_USERS_DRIVER=ocis|s3ng`.

Local originals and S3 metadata use distinct named volumes. S3 mode still requires persistent POSIX metadata. Artifact categories are separate `markdown/`, `images/`, `manifests/`, `previews/`, `tmp/` prefixes, managed by the application. The oCIS S3 key layout is exclusively owned by the official driver; this deployment invents no object-prefix or deduplication extension.

Mode changes do not migrate bytes or metadata. `setup.py` refuses to silently switch an existing configuration. A new isolated Compose project is appropriate for testing the other backend; production migration needs a separate verified plan.

Versioning stays enabled (`OCIS_DISABLE_VERSIONING=false`). Native global maximum quota is unlimited (`OCIS_SPACES_MAX_QUOTA=0`); the business layer owns the latest-original-only quota including business trash. The combination still needs actual quota acceptance.

Native personal/project trash purge cutoffs are set to `0` (disabled), and no revisions purge task is scheduled. Business deletion is MOVE, never a storage cleanup DELETE. Do not run native revision deletion, lifecycle expiration, volume deletion or generic orphan cleanup against retained originals/final artifacts. Cleanup may target only verified abandoned attempt/temp data or rebuildable preview caches. Never run `docker compose down -v` on a real deployment.

## Upload and proxy limits

- Admission: 20 GiB (`21,474,836,480` bytes).
- Requested large-file acceptance: exactly 10 GiB (`10,737,418,240` bytes), real Uppy+tus interruption/resume and final SHA256 comparison.
- Nginx body limit 20 GiB; request/response buffering disabled for tus and controlled file routes, timeout 3600 seconds.
- oCIS frontend maximum chunk size 100 MiB, multipart enabled, S3 part size 16 MiB, upload expiration 86400 seconds.
- Worker automatic conversion limit 100 MiB/500 pages; exceeding it must not undo a successful original upload.

These settings are initial configuration, not evidence that 10 GiB passed. Uppy/native transfer headers, offsets, exact checksums and recovery behavior must be recorded during the single required browser acceptance run. Cross-node upload takeover is not promised.

## Anonymous feedback and logs

Gateway access logs are disabled globally so feedback requests cannot be correlated through a per-request access record. The feedback route additionally strips forwarded IP, request/trace/baggage, referrer, user agent and cookies while retaining only authentication needed for transient permission checks. It has no proxy error-body log. PostgreSQL parameter/statement logging is off. Keycloak/kkFileView request logging is off; normal app logging must also honor the dedicated feedback path.

Do not enable APM request recording, tracing or CDN access logs over feedback/MCP paths. The deployment controls only its own chain: Peri/client-side logs need independent verification. Date-only feedback storage is not sufficient if a reverse proxy records identity/body/precise submission times.

## Operations and test commands

```sh
# Graceful stop/start, preserving all named volumes.
docker compose stop
docker compose start
# Dependency status. Do not paste logs containing secrets/documents.
docker compose ps
# Re-render generated/nginx.conf after a gateway template upgrade, using the
# same flags as the original setup. up.sh refuses to start if it is stale.
python3 setup.py --mode s3 --public-url https://drive.example.com \
  --trusted-proxy-cidr <same-as-before>
# Idempotent migrations after a reviewed upgrade.
docker compose run --rm --no-deps maintenance /app/maintenance migrate
# Static deployment tests, no Docker daemon needed.
python3 -m unittest discover -s deploy/compose/tests -p 'test_*.py' -v
# Optional actual RustFS initializer integration (synthetic temporary data).
RUSTFS_BIN=/path/to/official/rustfs python3 -m unittest discover -s deploy/compose/tests -p 'test_*.py' -v
# Read-only manifest resolution, not container execution.
python3 deploy/compose/images/lock_images.py --output /tmp/new-image-lock.json
```

Conversion retry goes through the authorized business conversion endpoint and creates a new immutable artifact generation. Uploads with unknown remote outcomes must be reconciled by their operation/session, not blindly retried or have quota released on timeout. Do not edit PostgreSQL pointers to make a remote discrepancy disappear.

## Multi-node design boundary

Compose is a single-node reference, not HA. To split nodes, replace dependencies with authenticated shared addresses, use a compatible shared POSIX metadata filesystem for official oCIS s3ng instances (e.g. appropriately configured NFSv4), preserve same config/version/secret sets, assign unique node identities, and route each upload to its owning node. Independent local metadata volumes are not a shared cloud drive. PostgreSQL/JetStream/artifact fencing remain authoritative; scaling a process does not prove a safe cluster. No multi-node failover, capacity guarantee or disaster-recovery acceptance is claimed.

## Official sources

- [oCIS 8.2 storage-users settings](https://doc.owncloud.com/ocis/8.2/admin/deployment/services/s-list/storage-users.html), [external OIDC proxy](https://doc.owncloud.com/ocis/8.2/admin/deployment/services/s-list/proxy.html), [storage considerations](https://doc.owncloud.com/ocis/8.2/admin/deployment/storage/general-considerations.html).
- [Keycloak standard token exchange](https://www.keycloak.org/securing-apps/token-exchange).
- [RustFS Docker deployment](https://docs.rustfs.com/en/installation/container/docker).
- [kkFileView 5.0.3 source release](https://github.com/kekingcn/kkFileView/releases/tag/v5.0.3).
