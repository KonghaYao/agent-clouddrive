# Converter and deployment native acceptance

Executed against synthetic disposable test documents, not user files.

- Converter suite: 35 tests passed, including real AnyDoc 0.2.4 Office extraction, real PaddleOCR CPU inference through HTTP, scanned-PDF OCR recovery, and private kkFileView DOCX/XLSX/PPTX PDF-to-PNG output. See `converters-native-35-tests.log`.
- Deployment suite: 5 tests passed, including generated local/S3 Compose config validation with standalone Compose 2.39.4, setup idempotence, realm security checks and actual RustFS 1.0.3 SigV4 bucket initialization twice. See `deployment-native-5-tests.log`.
- Docker Engine was absent. No container image build, Compose startup or full Docker-stack acceptance is asserted.
- No vision model/provider was configured: missing descriptions remain explicit partial results. Office native chart/SmartArt not exported as raster remains partial, not falsely complete.

## Preview build provenance

Official source: https://github.com/kekingcn/kkFileView/releases/tag/v5.0.3

The checksum-pinned official source archive was built with Apache Maven 3.9.11 and Java 21.0.12.1. `kkfileview-force-pdf.sh` deliberately changes one native Office decision to disable spreadsheet HTML output, preserving the native LibreOffice conversion pipeline. This is a project build, not an unmodified official application image.

Native build succeeded with `-Dmaven.compiler.release= -Dmaven.test.skip=true -Dassembly.skipAssembly=true package`. The host JRE contains the compiler module but lacks the JDK `ct.sym`; compilation used source/target 21 and current Java21 classes. The Dockerfile instead uses a full pinned JDK21 Maven image and retains the upstream release21 setting. Upstream Maven unit tests were skipped, not claimed passed. The produced native application was then exercised by our actual Office HTTP integration test.

- Official source archive SHA256: `49ac85cd98e28df60fe2dd42cbd9cadc2a6461a29a5259ce186e24da3c45be74`
- Explicit PDF patch script SHA256: `fc97c0c8b5fe5283944fa55b6d4f7c7ce0aec246cefe220c593f4e647a254c4b`
- Tested native patched application JAR SHA256: `c49b5385289690d274b5556504afc80a01914fb9cc65b08606ebb262d2b47f8f`
- OCR model lock manifest SHA256: `e01cdb67ad05659f292bd6cefeadf6c953bbf799ef39e545133738147c59f750`

## Scope of runtime fixes

The real preview test discovered that kkFileView independently reads its physical `config/application.properties`; Spring additional-location alone did not apply all native settings. The image now merges upstream and hardened properties at the expected path for both readers. Upstream XLSX output ignored the requested PDF mode, motivating the explicit patch above. Tests assert PNG-only assets and removal of temporary source handles.

RustFS health can precede usable S3 initialization; the test now calls the same bounded retry routine as production rather than bypassing it. Both buckets must actually exist via successful signed create calls before a pass. No lifecycle expiry/deletion is configured.
