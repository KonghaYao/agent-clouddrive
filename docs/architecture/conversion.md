# Conversion and controlled-preview adapters

## What is implemented

`converters/adapter.py` exposes the project HTTP contract. It is not claiming that upstream AnyDoc or PaddleOCR supplies these endpoints. It runs the real Firecrawl AnyDoc 0.2.4 Rust Python extension for Office/RTF/CSV/EPUB/OpenDocument, separate pypdf/PDFium page processing, actual PaddleOCR CPU recognition, an explicitly configured OpenAI-compatible vision HTTP endpoint, and controlled raster preview. Plain UTF-8 text/Markdown uses an explicitly named passthrough engine.

Every HTTP execution runs in a fresh subprocess with a wall-clock timeout, CPU/address-space/file-size/open-file limits and one concurrent job by default. Docker also applies memory/PID/CPU caps. There is no task database in the adapter. The Go worker owns leases, fencing, retry state, source-version binding, final publication and indexing.

Pinned direct and transitive Python versions are in `converters/requirements.txt` and `converters/requirements-ocr.txt`. OCR model names and SHA256s are in `converters/model-lock.json`. The adapter validates inference/config/model parameter digests before using model files. A changed model is an error, not a silent recipe upgrade.

## HTTP contract

- `GET /healthz`: process health only; this is not proof that optional models or upstream preview providers are ready.
- `GET /v1/capabilities`: engine, formats, capabilities, input/output/page/time limits. Internal network only.
- `POST /v1/execute`: requires `Authorization: Bearer <shared converter token>`, supplied through `ADAPTER_TOKEN_FILE`; caller uses `DRIVE_CONVERTER_TOKEN_FILE`.

Example input:

```json
{
  "job_id": "example-job",
  "attempt_id": "1",
  "input": {
    "filename": "report.pdf",
    "media_type": "application/pdf",
    "source_version_id": "immutable-version-id",
    "content_base64": "<base64 bytes>",
    "sha256": "<optional expected lowercase SHA256>",
    "size": 12345
  },
  "recipe": {"max_pages": 500, "timeout_seconds": 600},
  "output_scope": "tmp/example-job/1/"
}
```

Exactly one of `content_base64` or `url` is required. A URL must match a server-configured scheme+host+port and canonical path prefix in `ADAPTER_SOURCE_URL_PREFIXES`. The request cannot set the allowlist, OCR URL, model URL, provider key, output filesystem path or a bucket. Credentials in URLs, redirects, local file URLs, traversal/double-encoding and arbitrary hosts are rejected. Deployment defaults only permit `http://api:8080/internal/conversion-input/`; inline input remains available if that optional ticket endpoint is not implemented.

`output_scope` must exactly equal `tmp/{job_id}/{attempt_id}/`. The adapter does not possess S3 root credentials and never publishes a final artifact. Output is bounded inline data for the Go worker to validate and store under its attempt prefix.

Example result shape:

```json
{
  "schema_version": 1,
  "status": "partial",
  "markdown": "<!-- page:1 block:p1-b01 -->\nRecognized content\n",
  "assets": [{"id": "page-1-1.png", "media_type": "image/png", "content_base64": "...", "sha256": "...", "page": 1}],
  "pages": [{"page": 1, "block": "p1-b01", "text_status": "ready", "description_status": "failed"}],
  "completeness": {"status": "partial", "total_pages": 1, "missing_text_pages": [], "missing_description_pages": [1], "warnings": []},
  "errors": [{"code": "provider_not_configured", "stage": "description", "page": 1, "retryable": false}],
  "engine": {"name": "pdf-page-pipeline", "version": "pypdf-6.10.0/pdfium-5.14.0"}
}
```

Markdown references use `asset:{id}`. They are logical references, not public raw URLs. The worker must rewrite them to its authorized asset representation when appropriate. Each page marker precedes every asset/text/description on that page, including pages with failed OCR. `recipe.page` optionally selects one global 1-based PDF page, preserving the original page number and total document page count. Worker page-unit scheduling/persistence is separate from this adapter.

Status values: `ready`, `partial`, `not_converted`, `blocked`, `failed`. Error classes include `limit_exceeded`, `unsupported_format`, `encrypted`, `malformed`, `resource_limit`, `timeout`, `provider_not_configured`, `model_checksum_mismatch`, `temporary_failure`, `source_not_allowed`, and checksum/protocol errors. A successful HTTP response can carry a failed/partial conversion; inspect the body.

## Pipeline and bounds

Default input limit: 100 MiB. Default JSON output limit: 64 MiB. Default page limit: 500. Default execution wall clock: 600 seconds. Default decoded image cap: 24 million pixels. Default expanded ZIP cap: 256 MiB/10,000 entries. These are independent of the 20 GiB original upload admission limit.

PDF text is extracted per page. A page with fewer than eight non-whitespace characters is conservatively routed to OCR. Images/XObjects and detected vector drawing operations require a description stage. Page images are rasterized to PNG and persist as necessary reference assets. This is a conservative visual heuristic, not a certified semantic chart detector. Vision absence or failure is represented as missing descriptions; OCR is not substituted for image description.

AnyDoc Office formats have a logical document unit, not reliable printed-page coordinates. They therefore explicitly report `logical_document_unit_not_printed_page`. Native embedded image bytes are re-encoded as safe PNGs and referenced in Markdown. Remote embedded images are not fetched and yield a partial result. Embedded non-image objects are never executed. Office charts without extracted raster assets are not a validated description path; see the format matrix below.

AnyDoc's hosted OCR mode is always disabled (`ocr="reject"`). OCR uses the private PaddleOCR adapter. Vision uses only configured `DRIVE_VISION_BASE_URL`/model/key. External host use also requires `DRIVE_VISION_ALLOW_EXTERNAL=true` and an explicitly allowed outbound network; the default Compose internal network has no internet egress. No model credential, URL ticket or document body is logged.

Recipe fingerprint inputs must include: adapter `1.0.0`, AnyDoc `0.2.4`, pypdf `6.10.0`, PDFium binding `5.14.0`, Pillow `12.3.0`, CairoSVG `2.8.2`, PaddleOCR `3.3.2`, PaddlePaddle `3.2.2`, model lock SHA256 `e01cdb67ad05659f292bd6cefeadf6c953bbf799ef39e545133738147c59f750`, effective limits, configured vision model/provider identity (not key), prompt template `image-description-v1`, and Markdown schema `1`. Old published recipes/outputs must never be silently replaced.

## Controlled preview

`ADAPTER_MODE=preview` uses the same request envelope and returns only raster PNG assets. PDF rendering, raster image re-encoding and allowlisted static SVG rendering are implemented. SVG scripts/event handlers, external hrefs, CSS imports, foreign content, DTD/entities and URL fetches are rejected; no SVG or HTML is returned.

Office preview calls private kkFileView 5.0.3 using a fresh 256-bit temporary source handle served only on the internal network. The returned private converted PDF is validated against the fixed kkFileView origin/path, fetched without redirects, then rasterized. The adapter does not execute kkFileView HTML or send its page/PDF/source URL to a browser. Temporary source files disappear after the request. kkFileView's cache contains only rebuildable preview material, not retained originals.

The private backend is a project-built image, not an official kkFileView image. Its official source archive is SHA256-pinned. `kkfileview-force-pdf.sh` applies one deliberate source change: set the native Office `isHtmlView` decision to false. Unmodified v5.0.3 otherwise forces spreadsheet conversion to HTML even when PDF is requested. The patch makes DOCX/XLSX/PPTX uniformly convertible to PDF; it does not parse/reimplement Office. The response engine version identifies this build as `5.0.3+clouddrive-pdf.1/1.0.0`. The entrypoint merges upstream and hardened properties into the physical configuration file read by both Spring and kkFileView's own configuration loader.

The Go API must re-check current account, sharing state, lifecycle and exact current source version on every delivered pixel request. A cached raster is not authorization. The adapter itself is not a public sharing endpoint. Pixel saving/screenshots cannot be prevented.

## Reproducible tests

```sh
python3 -m venv .venv-converters
. .venv-converters/bin/activate
pip install -r converters/requirements-test.txt
python -m unittest discover -s converters/tests -p test_adapter.py -v
python converters/samples/generate.py
```

Actual OCR test (the optional test is skipped, not passed, without these prerequisites):

```sh
pip install -c converters/requirements.txt -r converters/requirements-ocr.txt
PADDLE_MODELS_DIR="$PWD/models" python converters/prepare_models.py
RUN_REAL_OCR=1 PADDLE_MODELS_DIR="$PWD/models" python -m unittest discover -s converters/tests -p test_real_ocr.py -v
```

Tests require DejaVu Sans for the synthetic OCR fixture. Production OCR does not depend on that test font. Normal Compose model preparation is `docker compose --profile setup run --rm prepare-models` before OCR-dependent acceptance. This is an explicit model download step; ordinary conversion containers stay offline. Model preparation is idempotent and checksum-verified.

## Actual format/test matrix, 2026-10-23

| Sample | Actual result | Evidence boundary |
| --- | --- | --- |
| UTF-8 TXT/Markdown | Ready | Exact Unicode passthrough asserted |
| CSV, RTF | Ready | Real pinned native AnyDoc, not mocks |
| DOCX, XLSX, PPTX | Ready, logical document unit | Synthetic files generated by their real format libraries, real AnyDoc extraction |
| DOCX embedded PNG | Partial without vision | PNG preserved/checksummed; missing description unit recorded |
| Text PDF, 3 pages | Ready | Actual text extraction, page markers and true page numbers |
| Image-only PDF | Partial | Actual CPU PaddleOCR recovers printed text; missing vision page is explicit |
| 200-page PDF, five image-only pages | Partial | Actual PDF, intentionally absent OCR/vision endpoints; exact five missing pages. This is page-accounting validation, not 200-page OCR throughput acceptance |
| PNG image OCR | Ready OCR stage | Real PaddleOCR HTTP subprocess recognizes “CloudDrive OCR test 12345” and “Stable source version 2026” |
| PDF/raster preview | Ready | Actual PNG output and metadata stripping checked |
| Static SVG preview | Ready | Actual CairoSVG raster; active/external content rejection tested |
| Office kkFileView preview | Real native HTTP pass: DOCX/XLSX/PPTX | Pinned-source backend + documented PDF-only patch + LibreOffice → PDF → PNG, temporary source cleanup checked; container runtime remains untested |
| DOC/XLS/ODT/ODS/ODP/EPUB and other AnyDoc-advertised extensions | Advertised adapter range, untested samples | Not claimed fully validated |
| Real external vision service | Not configured | Request/response/error handling exists; semantic description acceptance not performed |

There are 33 standard converter/security tests, one actual OCR integration test and one actual native Office-preview integration test (covering three formats). OCR ran successfully with the checked-in model hashes. Native chart/SmartArt objects not emitted as images by AnyDoc explicitly produce `partial`/`native_visual_object_not_rendered`, not a false claim of complete visual coverage. Test logs are captured separately in delivery evidence. Docker image startup, the whole Compose path and 10 GiB upload are separate acceptance items, not implied by these passes.

The optional native preview test requires `KKFILEVIEW_JAR`, `KKFILEVIEW_APP_CONFIG` (upstream properties) and optionally `KKFILEVIEW_OFFICE_HOME`. It launches its own isolated Java/LibreOffice and adapter processes using synthetic documents; without these settings it reports a skip rather than a pass. The upstream Maven test suite is not claimed to have run: the source build used `-DskipTests` and our own preview integration exercised the resulting application.

## Upstream and licenses

- [Firecrawl AnyDoc](https://github.com/firecrawl/anydoc): MIT; native 0.2.4 Python package, no hosted OCR.
- [pypdf](https://github.com/py-pdf/pypdf): BSD-3-Clause.
- [pypdfium2](https://github.com/pypdfium2-team/pypdfium2): Apache-2.0/BSD-3-Clause; bundled PDFium carries its own third-party notices. No PyMuPDF/AGPL dependency is used.
- [PaddleOCR](https://github.com/PaddlePaddle/PaddleOCR): Apache-2.0; official model notices must accompany redistribution.
- [CairoSVG](https://cairosvg.org/): LGPL-3.0; preserve applicable notices and library replacement rights.
- [kkFileView 5.0.3 security release](https://github.com/kekingcn/kkFileView/releases/tag/v5.0.3): Apache-2.0 application; LibreOffice and bundled conversion libraries have separate licenses. Upstream includes non-Office/CAD dependencies with their own restrictions; this project does not promise support or license clearance for every upstream feature.
