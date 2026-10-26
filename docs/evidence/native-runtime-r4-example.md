# Native runtime restoration for the example (2026-10-23)

The clean workspace was restored from the source package; no prior runtime was assumed. The official artifacts and pinned SHA256 checks in `native-runtime-r3.md` were replayed for Go 1.25.3, PostgreSQL 17.11, NATS 2.15.1, Meilisearch 1.55.0 and sqlc 1.29.0. All pinned hashes matched, and all executable version checks passed. This replay uses the previously recorded checksums; it does not claim to have independently re-fetched Debian signed metadata. sqlc's recorded hash remains a reproducibility hash, not an upstream checksum claim.

`native_identity_storage.sh` also restored official Keycloak 26.8.1 and oCIS 8.2.2 releases, using installed Java 21. A fresh synthetic realm, random synthetic credentials and local test certificate were generated outside the source repository. The key material and credential files are private (0600), under the external runtime directory. No user account or real-user persistent grant was created.

## Actual checks in this replay

- PostgreSQL started; `SELECT version()` reported 17.11; Unix domain socket directory configuration was empty.
- Native NATS health returned ready and Meilisearch health returned available.
- The repository's PostgreSQL invariant suite passed against this real server.
- Keycloak realm import and HTTPS OIDC discovery completed with certificate validation.
- oCIS HTTPS status completed with certificate validation.
- All validation processes were stopped after checking, leaving the fixture ready for the application's own same-execution launch.

The runtime wrapper is regenerated from the exact documented heredoc in `native-runtime-r3.md` as `../dependency-runtime/with-native-deps.sh`. The application/example validation must run inside that invocation, not in a separate network namespace:

```sh
../dependency-runtime/with-native-deps.sh bash -c '
  source scripts/native_identity_storage.sh
  native_identity_prepare
  native_identity_start
  trap native_identity_stop EXIT
  # Launch the example/application and its actual verification here.
'
```

Application implementation owns the subsequent seed, content, frontend and API validation. This restoration alone does not establish example acceptance, browser operation, Docker Compose execution, S3 validation, or conversion acceptance. The local storage native fixture uses the oCIS local originals driver. Runtime, downloads, secrets and private keys must never be copied into source archives.
