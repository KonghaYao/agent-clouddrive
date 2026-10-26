# Controlled audit access and anonymous feedback

## Audit is not administration

`reviewer_grants` permits processing-job metadata or feedback triage only. It does not grant original/Markdown access. `audit_grants` is a separate, explicit `(principal_id, file_id)` authorization with grantor and optional expiry, provisioned by the deployment's authorized operator. There is no self-grant API, implicit administrator content bypass, share-link upgrade, or MCP audit tool.

Every audit request requires an active authenticated human web-client principal and a current grant. REST-only routes:

- `GET /v1/audit/files/{id}/versions`
- `GET /v1/audit/files/{id}/content?version_id=<fixed source>`
- `GET /v1/audit/files/{id}/markdown?artifact_set_id=<fixed artifact>`
- `GET /v1/audit/files/{id}/artifacts/{artifact}/assets/{asset}`

Version/artifact IDs must belong to the granted file. Asset IDs must occur in that exact immutable manifest. Expired/revoked grants, Agents, other files and arbitrary keys are denied. Original downloads and audit assets are attachments; Markdown is plain text. Each authorized read is recorded separately in `audit_reads`; no raw access tokens, URLs with credentials or content bodies are recorded.

Business trash does not destroy retained sources or artifacts. Auditing original bytes still uses the legitimate existing owner delegation, never an administrator impersonation token. If that delegation is absent, revoked or the owner is disabled, original access fails closed. Explicit audit can still inspect independently stored retained Markdown/assets. Native original audit after owner-disable remains an integration limitation requiring an independently authorized native mechanism.

## Anonymous feedback

The `feedback` table contains only random UUIDv4, date received, relevant file/version/artifact identifiers, category/body and processing state. No author, agent, session, IP, trace/request ID or precise creation time is stored. API/MCP feedback uses read-only transient authentication: no principal last-seen update, quota update, ordinary audit entry or outbox event. Even rejecting a disabled account through this path does not persist an identity activity timestamp.

Gateway request logging is disabled, identity-server access/event logs are disabled in the reference configuration, and the feedback path strips correlation headers. Do not add APM body capture, identified spans, proxy access logs or database parameter logging. A global-per-process 60-submissions-per-minute in-memory window and request-size limits reject excess submissions without tracking principal/IP/session or persisting identity. Feedback bodies may themselves identify their writer; the UI explicitly warns against including personal identity.

These are controlled-server guarantees, not proof about Peri/client logs or an external hosting provider. The complete chain must be reviewed in the actual deployment before claiming full unlinkability.
