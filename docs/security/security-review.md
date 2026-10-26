# Independent correctness and security review

Reviewed on 2026-10-23 against the v1 engineering handoff. This is a focused source and database-protocol review, not a penetration-test certification or complete deployment acceptance.

## Verdict

The reproduced defects listed below have been corrected. The final focused run passed 10 tests, repeated three times, with Go's race detector and a real PostgreSQL 17 server. No remaining reproducible high-impact defect was identified in the reviewed paths.

Final evidence: `evidence/security-review-final.log`.

## Reproduced findings and corrections

1. **A new upload could enter an in-flight MOVE target.** A native MOVE paused after its intent was committed, then a new tus upload was admitted at the same target path under another file ID. The native MOVE succeeded but its database commit failed with `PATH_CONFLICT`. Upload admission now takes the same space serialization lock and checks unresolved target-path intents before creating its provisional file. The native upload call is not made when the target is reserved.
2. **An explicitly approved historical anonymous link could not open.** Its initial request defaulted to the current source version before applying the fixed-version grant. Blank initial requests now select the granted version; explicit requests for another source remain denied.
3. **A fixed historical received-share listing exposed newer source metadata.** The listing returned the file's current version ID and size even when only an older source was authorized. Shared listings and stat responses now project metadata from the exact granted source and omit internal operation/search-artifact pointers.
4. **Pinned owner Markdown stopped resolving after retained deletion.** An explicit artifact citation used the active-file gate even though the source and artifact remained stored. Explicit historical Markdown and referenced assets now use owner history authorization; unrelated principals remain denied.
5. **Cancellation before native upload stranded quota and the path.** If the request was canceled after admission but before credential resolution completed, cleanup used the canceled request context. The operation stayed `prepared` with held quota despite no native call. Confirmed pre-dispatch failure cleanup now uses a separately bounded context and releases the reservation/path without replaying a native mutation.

Initial failing evidence is intentionally retained in `security-review-regressions-initial.log`, `security-review-target-race-initial.log`, and `security-review-cancellation.log` under `evidence/`. The final log supersedes their failure status.

## Additional verified protections

- Upload admission reloads its parent after space serialization rather than persisting a stale path after a concurrent parent rename.
- Share approval racing file deletion or account disable leaves no active grant. Restore/re-enable does not revive an earlier confirmation.
- Accepted and rejected feedback authentication/submission does not change principal activity state or create a correlating outbox record.
- The previously missing independent audit application routes now require an active human and an unexpired, explicit per-file audit grant. Generic reviewer/admin privileges and Agent identity do not grant file-content audit access.
- Audit originals require an explicit source ID; Markdown/assets require an exact published artifact belonging to the granted file. Asset access is limited to manifest members and rejects path traversal. Successful audit reads are recorded separately from anonymous feedback.
- Explicit audit can read retained artifacts after business deletion and owner disable. A disabled auditor or expired grant is denied. Native original audit still requires the owner's legitimate delegated credentials; no administrative impersonation is introduced.
- Native original reads compare the response ETag before exposing the body, because oCIS GET may ignore `If-Match`. Current-source ETag pins resolve historical revisions after overwrite rather than assuming the current source appears in the historical revision listing.

Tests added by this review are in `internal/application/security_review_test.go`. Existing privacy and share-race tests were also rerun.

## Exact test boundary

The focused tests execute real PostgreSQL migrations, transactions, constraints, triggers, advisory locks, and durable state. Artifact tests use the real immutable local artifact store. Native oCIS calls and identity/delegation are explicitly labeled application-contract fixtures in this test layer. The ETag implementation was source-reviewed here; consult the separate native adapter evidence for live oCIS tests.

This review does **not** establish successful Docker Compose startup, live browser interaction, Peri OAuth interoperability, full production-chain anonymous-log isolation, or the required 10 GiB Uppy+tus interrupted/resumed transfer. It also does not turn native tus creation-time conditions into final-commit CAS: safety depends on the application enforcing exclusive operations and target-path admission, and on keeping unsupported direct native writes outside the product surface. Full acceptance must use the project's acceptance matrix and the relevant live integration evidence.

## Re-run

With `DRIVE_TEST_DATABASE_URL` pointing only to a disposable PostgreSQL instance, run from the project root:

```sh
go test -race -count=3 -v ./internal/application \
  -run '^TestReview|^TestFeedbackLeavesNoIdentityActivityRecord$|^TestShareApprovalConcurrent'
```

Without the database endpoint, database tests skip rather than prove these properties. The aggregate self-test and generated-code checks must also be rerun after any later production or dependency changes.
