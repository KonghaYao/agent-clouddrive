# Real native application core-loop acceptance

Date: 2026-10-23, passed at approximately06:56 UTC.

Actual services: application Go binaries, PostgreSQL17.11, official Keycloak26.8.1, official oCIS8.2.2 local driver, NATS JetStream2.15.1, Meilisearch1.55.0. Artifact store local; direct text conversion path. One synthetic account in an isolated realm, HTTPS IdP/oCIS verified using known test CA. No identity/original/search/message mock adapters in this loop.

Verified sequentially:
1. Real user login/consent and S256 PKCE authorization-code exchange.
2. Application /v1/me authenticates signed subject-bearing JWT and current IdP status.
3. Explicit application background-delegation start, actual IdP consent and real application callback; encrypted refresh-vault status enabled.
4. Controlled upload admission with durable operation, tus PATCH, saved finalization with source-version ID.
5. Official original content download equals the uploaded UTF-8 Chinese/plain-text bytes.
6. Real outbox/JetStream/worker pipeline reads source via offline delegation, publishes immutable text artifacts and Meilisearch index.
7. Search contains the EXACT just-created file_id and version, not merely an earlier matching document. Initial probe was tightened to enforce this distinction, then rerun successfully.

The first attempt revealed missing Keycloak basic/sub scope; corrected in realm template. Earlier initial probe expected finalization state `committed` whereas API correctly returns `saved`; probe was corrected, not application behavior.

Reusable HTTP protocol probe: tests/integration/native_application_acceptance.py. Supply a running disposable stack, test realm redirect configuration, TEST_PUBLIC_URL/TEST_OIDC_ISSUER/TEST_CREDENTIALS_FILE and optional TEST_CA_FILE. Native service setup used extracted official binaries because Docker is unavailable here; this is NOT a Docker Compose acceptance run.

Not covered by this record: all format paths, real Peri client, production identity lifecycle, user browser interactions, full A01–A24 matrix, 10GiB browser interruption/resume, S3 application mode (separate adapter/oCIS smoke evidence exists), fault recovery or multi-node behavior. Do not interpret a core-loop pass as complete specification acceptance.

## Expanded real native verification (approximately07:08 UTC)

With SDK1.8.0 and two distinct real Keycloak test users:
- MCP2026-07-28 server/discover, tools/list and tools/call succeeded with namespaced per-request metadata and required Mcp-Method/Mcp-Name headers. Legacy initialize separately negotiated2025-11-25. This is an actual HTTP protocol probe, not the Peri application.
- Competing overwrite was rejected409 while the first upload owned the operation; saved new original and exact historical original both read correctly.
- Business deletion performed native MOVE, ordinary original access returned404, and restore preserved both current and old original bytes.
- Second user could neither stat nor download the private file and received no private search results; read-only scope rejected directory creation403.
- Sharing approval without interactive CSRF was rejected403. Actual authenticated UI-session+CSRF approval created a read/download grant; recipient REST download succeeded, but the same recipient's MCP file lookup was denied.
- These checks repeated the complete direct-text pipeline and required the newly created file ID in search results.

Exact sanitized runtime output: evidence/native-full-application-final.log. The retained generated files and IDs belong only to disposable test users. No authorization URLs, passwords, tokens or private keys are recorded in that evidence.

The shipped portable `native_application_acceptance.py` was itself executed against the real native stack after parameterization, with two disposable accounts and explicit test-write opt-in. It passed the expanded core/lifecycle/isolation/sharing checks and MCP2026-07-28 discovery/list/tool invocation. Exact output: `evidence/native-portable-acceptance-final.log`. This avoids relying solely on an unshipped ad-hoc development script.

Final frozen-source rerun passed (approximately07:13 UTC): application binaries rebuilt after prepared-upload cancellation recovery and feedback rate-limit hardening; shipped portable probe repeated all preceding scenarios. Additional exact two-reader checks passed: after the first overwrite committed, a request carrying the original stale source-version/revision was rejected409; replaying the same original idempotency request returned the same upload ID; repeated finalization returned the same source-version ID. Neither extra version nor silent stale overwrite was accepted.
