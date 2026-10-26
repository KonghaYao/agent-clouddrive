# Known limitations and release blockers

This document distinguishes a runnable implementation from a complete acceptance claim. The exact current gate statuses and evidence are in `acceptance.json`; release readiness fails unless every mandatory gate is actually passed.

## Environment-dependent acceptance still required

1. **Full Docker Compose runtime:** no usable Docker/Podman daemon or permitted container runtime in the implementation workspace. Static Compose validation and actual native dependency tests passed, but they do not prove container startup, mounts, health dependencies, egress policy or image compatibility.
2. **Real Chromium + 10 GiB Uppy upload:** Chromium cannot create required sockets in this execution environment. The approved alternate execution attempt also failed before browser launch. Actual Uppy tus transport/DOM tests passed, but no real-browser large-file acceptance is claimed. Execute the spec's exact 10,737,418,240-byte transfer, interrupt/resume, final checksum and memory/offset assertions on an authorized working browser/host. Native or fake-payload traffic is not a substitute.
3. **Real Peri integration:** official MCP SDK client/server interoperability and OAuth resource/scopes are tested separately; a real Peri version, redirect and consent/logging behavior must still be configured and verified. The reference Peri client's redirect list is intentionally empty, not wildcarded.
4. **Vision description:** no external model endpoint/key was supplied. The HTTP adapter exists and failures are explicit `partial` with missing description pages. OCR text is never represented as a substitute for semantic image description. No third-party document transmission was performed.
5. **Office preview and full format coverage:** consult the current converter evidence. AnyDoc text extraction does not guarantee visual fidelity for native Office charts/SmartArt; unrendered visual objects are reported partial, not ready. Formats without actual samples remain unverified.

## Intentional v1 limits

- Single-node reference only. No cluster, cross-node tus takeover, failover, load test, backup/recovery or capacity guarantee.
- Original upload admission is 20 GiB; automatic conversion is independently bounded to 100 MiB/500 pages. Successful originals remain downloadable when conversion is too large, unsupported or partial.
- The source/artifact pipeline buffers only bounded conversion inputs/outputs. Upload/download originals stream; finalization performs a complete pinned original read to verify SHA256, so finalizing a large file includes that extra I/O cost.
- Single standard byte ranges are supported; suffix/multipart byte ranges are not.
- Temporary/orphan cleanup is conservative. An ambiguous remote write keeps its reservation/gate until reconciliation; the system does not guess failure, delete retained data, or forcibly clear quota to appear healthy.
- Published conversion outputs are immutable. Reconvert creates a new generation/recipe; old references continue to refer to their original source/artifact. No cross-user deduplication/reuse.
- Audit content is a separately authorized human REST surface. Retained Markdown/assets can be read under an unexpired explicit per-file grant even when the owner is disabled. Reading an original still requires a lawful working owner's offline delegation; a revoked/disabled owner grant fails closed. An independently authorized native audit credential mechanism is **not** implemented and no admin impersonation workaround exists.
- Feedback has no source identity columns and uses date-only receipt. The controlled gateway/API/DB/outbox paths avoid identity correlation, but client/Peri logs, hosting/network observation, and identity information voluntarily typed in feedback cannot be certified by server tests.

## Integration semantics established by real probes

The tested oCIS 8.2.2 current source is not listed as a historical revision until overwritten; stable resource ID plus ETag captures it. GET ignores a stale request `If-Match`, so the adapter checks the returned ETag before exposing bytes and falls back to matching historical metadata. Two already admitted native tus sessions can both finish even with the same admission ETag. Therefore official native final CAS is **not** claimed. The application prevents such admission with a durable full-transfer file gate and per-space target reservations, and keeps official oCIS inaccessible directly to clients.

See `docs/evidence/ocis-native-m0.md` for the actual local and RustFS/s3ng counterexamples and checks. These constraints must be re-probed after upgrading oCIS, changing the identity provider, or changing the official storage driver.
