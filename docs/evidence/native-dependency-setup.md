# Native dependency setup (2026-10-23)

This is actual local dependency startup, NOT a Compose/full-system acceptance result.

- PostgreSQL 17.11, official Debian trixie packages `postgresql-17_17.11-0+deb13u1_amd64.deb` and matching client, extracted without system install. initdb completed. TCP-only localhost:55432 startup and CREATE DATABASE clouddrive_test succeeded. Unix sockets are unavailable in this executor; ordinary PostgreSQL `unix_socket_directories=''` configuration used. Test-only trust authentication, never production defaults.
- NATS Server 2.15.1 official nats-io GitHub release, native JetStream startup succeeded at loopback:54222 with filesystem persistence. Test only, no external listener.
- Meilisearch 1.55.0 official meilisearch GitHub release, native startup succeeded at loopback:57700, development mode, telemetry disabled. Test only, production requires a secret.
- Docker/Podman not installed; no container socket or effective Linux capabilities reported. No security settings changed. This record does not establish oCIS/Keycloak/RustFS/Peri integration or the 10 GiB browser test.

Sources:
- https://deb.debian.org/debian/pool/main/p/postgresql-17/
- https://github.com/nats-io/nats-server/releases/tag/v2.15.1
- https://github.com/meilisearch/meilisearch/releases/tag/v1.55.0

Dependency binaries and data directories are outside the deliverable repository and must not be included in the source ZIP. Application integration results must be recorded separately with test names and output.
