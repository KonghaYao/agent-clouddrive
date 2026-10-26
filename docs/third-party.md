# Dependency and redistribution notes

This delivery contains project source, lockfiles, generated SQL bindings, synthetic test samples and sanitized evidence. It does not redistribute dependency executables, OCR model weights, oCIS/RustFS data, private certificates, credentials, browser binaries, Maven caches or npm modules.

Dependency pins and authoritative upstream identifiers:

- Go modules: `go.mod` / `go.sum`, including CloudWeGo Hertz, official `github.com/modelcontextprotocol/go-sdk` **v1.8.0**, pgx and NATS. `sqlc.yaml` plus the self-test runner pin sqlc **v1.29.0** generation.
- Browser dependencies: `web/package.json` / `web/package-lock.json`, including React, Ant Design and Uppy.
- Python/native conversion: `converters/requirements*.txt`; model identities/checksums: `converters/model-lock.json`.
- Container images: `deploy/compose/image-lock.json` records actual official registry manifest lookups; Compose and Dockerfiles use returned digests. Native integration versions are recorded separately from image execution.
- kkFileView: official v5.0.3 source archive SHA256, build procedure and any narrowly scoped source patch are recorded in its Dockerfile and conversion documentation. This is not a claim that an unavailable upstream application image was successfully pulled.

Operators building images must preserve the licenses and third-party notices delivered with upstream packages and model artifacts. The source package does not remove or supersede those terms. `docs/conversion.md` identifies the converter libraries with potentially distinct notice requirements, including PDFium, CairoSVG and LibreOffice. No blanket license-clearance or support claim is made for every upstream advertised format.

Official MCP v1.8.0 supports protocol versions 2026-07-28, 2025-11-25, 2025-06-18 and 2025-03-26. The generated `docs/mcp-tools.json` obtains its protocol list directly from the pinned SDK. The actual current SDK integration test asserts negotiation of 2026-07-28. Real Peri client compatibility remains a separate acceptance gate.
