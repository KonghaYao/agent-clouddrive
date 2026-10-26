# oCIS native integration findings — 2026-10-23

Actual official oCIS 8.2.2+fbd21b63 native process, local driver, one disposable user's personal space. TLS verified using the generated test server certificate; no certificate-verification bypass. Own test admin account Basic authentication used for this isolated protocol probe, NOT administrator impersonation and NOT production OAuth/delegation acceptance.

## Confirmed native behavior

- Graph `/graph/v1.0/me/drives` discovers personal space.
- POST parent `/dav/spaces/{parent}` with filename metadata, Upload-Length, Tus-Resumable creates upload (201).
- PATCH signed same-origin `/data/...` completes (204), returning `Oc-Fileid` stable full resource ID and `Upload-Offset`.
- Stable-ID PROPFIND/GET works. Fresh current content has no entry in `/dav/meta/{id}/v`; previous content becomes a revision only after overwrite.
- After overwrite, PROPFIND revision list exposes old ETag and immutable revision href. GET that href returned the original bytes.
- Official MOVE to a business-trash directory preserved resource ID, current bytes, and the prior revision list.

## Critical counterexamples requiring business-layer protections

1. GET stable resource with stale If-Match returned HTTP 200 and NEW content. Never trust If-Match alone for fixed reads. Verify response ETag against the pinned ETag before returning or processing any response body. If mismatched, close it, resolve the exact matching old revision and read that revision. Do not use current-path fallback.
2. Two tus sessions started with the same original If-Match were both admitted (201). PATCH A and PATCH B both completed (204), and GET returned B. Native tus does not provide the assumed final-commit CAS. The business database must retain exclusive file-operation ownership from upload admission through reconciliation, reject other content/move/delete operations, and expose no direct native bypass. Unknown outcome cannot release the lock.

These observations validate endpoints and reveal hazards, but do not establish complete G01/G03–G05 acceptance: real Keycloak delegated identity, application fencing/concurrency, unknown-outcome reconciliation, S3 driver and repeated end-to-end lifecycle remain separate tests.

Reproducible diagnostic: tests/integration/ocis_native_probe.py. Requires an explicitly configured disposable test account, OCIS_TEST_URL, OCIS_TEST_USER, OCIS_TEST_PASSWORD and OCIS_CA_FILE; it creates small test files and versions, never permanently deletes. Output omits Authorization and signed upload URLs. Do not use production credentials or package local generated credentials.

## S3 backend repetition

The same small-file protocol probe was repeated using official `s3ng` on oCIS8.2.2 with native RustFS1.0.3, a dedicated originals bucket and separate POSIX metadata directory. tus create/PATCH, overwrite, old-revision GET, stable-ID MOVE and retained revision listing succeeded. Both critical precondition counterexamples reproduced. This establishes a narrow real S3 compatibility smoke, not multipart/10GiB, business-layer acceptance or production credential handling. Native setup used `STORAGE_USERS_S3NG_PUT_OBJECT_DISABLE_MULTIPART=false`; the small payload alone does not exercise multipart.
