# Agent CloudDrive Web

React + Vite + TypeScript + Ant Design frontend. All file, quota, conversion, search, share and upload state comes from the business API. There are no seeded production files, mock login accounts or simulated successful uploads.

## Run

Requires Node.js 24 (the delivery build used 24.19.0) and npm. Dependencies are exact-pinned in package.json and package-lock.json.

```sh
npm ci
npm test
npm run build
npm run dev
```

Development uses port 5173 with `/v1` and `/s` proxied to the Go API on `127.0.0.1:8080`. For this development flow, set the backend public URL to `http://localhost:5173` (or the exact browser origin), and configure that same origin in the backend allowed-origins and the identity client's redirect URIs. Controlled upload URLs intentionally reject a different public origin. Docker serves `dist` through the gateway, with API, identity and frontend on one public origin.

## Runtime identity configuration

Serve this JSON at `/config.json`, with `Content-Type: application/json` and `Cache-Control: no-store`:

```json
{
  "oidc_issuer": "http://drive.localhost:8080/identity/realms/clouddrive",
  "oidc_client_id": "clouddrive-web",
  "oidc_audience": "agent-clouddrive"
}
```

An optional `oidc_resource` may carry an absolute resource URI when required by an alternative identity provider. Audience and resource are not conflated. The production gateway must also route `/auth/callback` to the SPA. Register `${publicOrigin}/auth/callback` and `${publicOrigin}/` as the login callback and logout redirect respectively. Never put a client secret in frontend configuration.

The login uses oidc-client-ts Authorization Code + PKCE (S256). All seven business scopes start selected and can be deselected. Access, ID and refresh tokens stay in memory; sessionStorage holds only transient PKCE/redirect state and a non-credential recovery marker (issuer, client ID, the business scopes actually granted, and a timestamp). It never holds tokens, the OIDC User, CSRF values or profile claims, and nothing is written to localStorage. The callback removes the authorization code from history. Re-authorizing a refreshed token does not unmount a running upload.

On a normal or hard refresh in a tab that completed login, the marker allows at most one bounded (`prompt=none`) iframe silent request against the existing identity-provider SSO session, reusing the registered `/auth/callback` with an iframe-only branch. Success restores the Drive without a click or password; a missing/expired SSO session, required consent, timeout or service failure falls back to the ordinary login page, with no top-level redirect loop and no automatic retry. The marker only replays the previously granted business scope subset and is untrusted UI input, never an authorization source. A new tab or first visit still requires one manual login. Logging out clears the marker, stops silent renew and ignores any late silent result. Silent recovery never approves a share: the explicit confirmation remains required.

After login, POST `/v1/session` obtains an HttpOnly SameSite security cookie and a CSRF token. Share approval requires a separate explicit checkbox and button, the fixed stored request, the web-client token, cookie, exact Origin and X-CSRF-Token. The browser never approves on behalf of an Agent automatically. Returned public links are displayed immediately because only their digest is retained server-side.

A separate, optional offline-delegation banner checks `/v1/delegation/status`. Only a user click invokes `/v1/delegation/start`. The resulting URL must match the configured identity provider's discovered authorization endpoint. The browser never handles the server's background refresh credential.

## Implemented workflows

- Own files: breadcrumbs, paginated listing, directory picker, create/move/rename, conditional metadata revisions, overwrite version guards, soft delete, independent batch delete results and failed-item retry.
- Upload: real `@uppy/core`, `@uppy/dashboard` and `@uppy/tus`; backend-created controlled session URL, 32 MiB PATCH chunks, HEAD offset recovery, pause/retry, token refresh before each request, and separate server-finalize/operation status. Upload bytes completing does not mean business commit completed. No native oCIS URL is accepted.
- File details: current metadata, fixed source versions, paginated Markdown, safe raster assets, controlled preview, manual reconversion and anonymous feedback. Markdown skips raw HTML, sanitizes the tree and dangerous links, and blocks external tracking images.
- Search: keyword results show actual returned source version, stable artifact reference, stale-current status and exact missing pages. Opening a hit preserves its returned version and artifact.
- Trash: original path/deletion time, explicit restore directory/name, conflict remains editable, no permanent-delete control.
- Sharing: exact system user/group ID or preview-only anonymous link; immutable confirmation, latest/fixed-history policy, expiry, revocation; received shares stay read-only and outside Agent/own-file search.
- Admin processing/feedback views are hidden without an explicit `permissions.admin` response. Backend authorization remains authoritative; frontend visibility does not grant content access.

For large downloads, Chromium's File System Access API streams the response into the chosen file with backpressure. Browsers without that API have a strict 100 MiB bounded fallback and must use a supported Chromium browser for larger originals. The code never buffers a 10 GiB original in memory. Closing an active upload panel ends the in-page client and may require server-side reconciliation/expiry; pause within the panel instead for ordinary network interruption.

## Verification

Executed in this delivery workspace:

- `npm ci`: exact lockfile install (also repeated before final verification).
- `npm test`: 50 tests passed at the initial verified checkpoint; the final command output is authoritative if more tests are added.
- `npm run build`: TypeScript check and production Vite build passed, with upload and Markdown code split into lazy chunks.
- Real DOM component tests cover scope selection, confirmation/expiry/cancel, folder navigation, stale-response rejection and malicious Markdown.
- Protocol tests instantiate the actual Uppy + tus browser implementation with a deterministic test HTTP stack. They verify HEAD-based nonzero-offset continuation, forbidden-session failure without success/retry, and prevention of authorization transmission to an external upload URL. This is a client protocol test, not oCIS/S3 integration or the 10 GiB acceptance test.

`npm run test:browser` is a reproducible Chromium smoke harness for a machine with supported local sockets and a normal Chromium security sandbox. It checks actual UI scope selection, outbound PKCE parameters, cancelled login recovery and desktop/mobile screenshots. `CHROMIUM_PATH` may specify an installed Chromium binary. It does not disable the browser sandbox.

Browser execution was attempted here but blocked before page creation: normal execution reported `process_singleton_posix socket() failed: Operation not permitted`; a formal escalated retry failed because the runtime could not create its read-only session mount. No Chromium security flags were relaxed, no screenshot was obtained, and browser smoke, live IdP/oCIS/Peri integration and the 10 GiB disconnect/resume acceptance test are not represented as passed.

Test fixtures exist only in `src/test` and the explicitly labelled browser harness. They are not included as authenticated production state.
