# Native identity and storage restoration (2026-10-23)

The source-package runtime was absent. Official Keycloak 26.8.1 and oCIS 8.2.2 Linux amd64 releases were downloaded again; system Java 21 is used. Disposable state and generated secrets remain outside the repository in `../dependency-runtime` by default.

## Reproduce

In one shell/execution namespace:

```sh
source scripts/native_identity_storage.sh
native_identity_prepare
native_identity_start
# Start the application and tests in this same execution namespace.
# Exported TEST_* / DRIVE_* variables include CA and private credential paths.
native_identity_stop
```

Set `NATIVE_RUNTIME` to use an alternative empty external directory. `NATIVE_PUBLIC_URL` defaults to `http://127.0.0.1:58081`; changing it after initialization requires fresh test state. No production accounts are imported. `owner` and `reader` are synthetic users with random generated passwords, and explicit offline-access roles. Keycloak binds loopback HTTP 58080/HTTPS 58443; oCIS proxy binds loopback HTTPS 59201. Test and application clients explicitly trust the generated seven-day test certificate. Certificate verification remains enabled. The application public URL is loopback HTTP for this isolated protocol harness, not a deployment recommendation.

The helper uses the repository realm generator, including basic/sub, profile, email and role scopes, separate confidential background-consent client, standard exchange and least-privilege identity lookup service account. The startup process is limited to a 768 MiB Java heap and oCIS GOMEMLIMIT=768MiB/GOMAXPROCS=2. These are process-local test settings, not host configuration.

## Observed checks

- Imported realm and Keycloak HTTPS discovery successfully.
- Started actual oCIS native services and fetched status over verified TLS.
- Completed real authorization-code flow with S256 PKCE and synthetic-user consent.
- Exchanged the user's access token with the confidential backend client: HTTP 200, audience `ocis`, role `ocisUser`.
- Requested `openid` in exchange and successfully discovered the user's real personal oCIS drive through the Graph endpoint, without administrator impersonation.

This is protocol-level native evidence, not browser acceptance, complete application stress acceptance, S3/multipart validation or production authorization. Offline application delegation is left to the application acceptance/stress driver. This restored oCIS setup uses its local originals driver; the S3 configuration remains separate.
