// Package config loads a fail-closed deployment configuration. No credentials are logged.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL, NATSURL, NATSStream, Listen, MCPListen, PublicURL, MCPURL, OIDCIssuer, Audience, WebClientID, OCISURL, OCISAudience, TrashDir, ArtifactRoot, StorageMode, MeiliURL, MeiliKey, MeiliIndex, AnyDocURL, OCRURL, PreviewURL, NodeID, PipelineVersion, VisionModel, VisionProvider, OCRModelManifest string
	AllowedOrigins                                                                                                                                                                                                                                                                                               []string
	UploadMax, ConvertMax, DefaultQuota                                                                                                                                                                                                                                                                          int64
	ConvertPages                                                                                                                                                                                                                                                                                                 int
	Lease                                                                                                                                                                                                                                                                                                        time.Duration
	AllowUnverifiedOCIS                                                                                                                                                                                                                                                                                          bool
	TusDiagnostic                                                                                                                                                                                                                                                                                                bool
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func integer(k string, d int64) int64 {
	v, e := strconv.ParseInt(env(k, strconv.FormatInt(d, 10)), 10, 64)
	if e != nil || v < 0 {
		return d
	}
	return v
}
func Secret(name string) (string, error) {
	p := os.Getenv(name + "_FILE")
	if p != "" {
		b, e := os.ReadFile(p)
		return strings.TrimSpace(string(b)), e
	}
	return os.Getenv(name), nil
}
func Load() (Config, error) {
	c := Config{DatabaseURL: env("DRIVE_DATABASE_URL", ""), NATSURL: env("DRIVE_NATS_URL", "nats://nats:4222"), NATSStream: env("DRIVE_NATS_STREAM", "CLOUDDRIVE"), Listen: env("DRIVE_LISTEN", ":8080"), MCPListen: env("DRIVE_MCP_LISTEN", ":8081"), PublicURL: env("DRIVE_PUBLIC_URL", "http://localhost:8080"), MCPURL: env("DRIVE_MCP_PUBLIC_URL", "http://localhost:8080/mcp"), OIDCIssuer: env("DRIVE_OIDC_ISSUER", ""), Audience: env("DRIVE_OAUTH_AUDIENCE", "agent-clouddrive"), WebClientID: env("DRIVE_WEB_CLIENT_ID", "clouddrive-web"), OCISURL: env("DRIVE_OCIS_INTERNAL_URL", "http://ocis:9200"), OCISAudience: env("DRIVE_OCIS_AUDIENCE", "ocis"), TrashDir: env("DRIVE_TRASH_RESERVED_DIR", ".clouddrive-retained"), ArtifactRoot: env("DRIVE_ARTIFACT_LOCAL_ROOT", "/data/artifacts"), StorageMode: env("DRIVE_STORAGE_MODE", "s3"), MeiliURL: env("DRIVE_MEILI_URL", "http://meilisearch:7700"), MeiliIndex: env("DRIVE_MEILI_INDEX", "clouddrive"), AnyDocURL: env("DRIVE_ANYDOC_URL", "http://anydoc:8090"), OCRURL: env("DRIVE_OCR_URL", "http://paddleocr:8090"), PreviewURL: env("DRIVE_PREVIEW_INTERNAL_URL", "http://preview:8090"), NodeID: env("DRIVE_NODE_ID", "single-node"), AllowedOrigins: strings.Split(env("DRIVE_ALLOWED_ORIGINS", "http://localhost:8080"), ","), UploadMax: integer("DRIVE_UPLOAD_MAX_BYTES", 20<<30), ConvertMax: integer("DRIVE_CONVERT_MAX_BYTES", 100<<20), DefaultQuota: integer("DRIVE_DEFAULT_QUOTA_BYTES", 100<<30), ConvertPages: int(integer("DRIVE_CONVERT_MAX_PAGES", 500)), Lease: time.Duration(integer("DRIVE_JOB_LEASE_SECONDS", 300)) * time.Second, AllowUnverifiedOCIS: os.Getenv("DRIVE_OCIS_INTEGRATION_VERIFIED") == "true"}
	c.PipelineVersion = env("DRIVE_CONVERTER_PIPELINE_VERSION", "clouddrive-converter/1.0.0;anydoc/0.2.4;pypdf/6.10.0;pypdfium2/5.14.0;Pillow/12.3.0;CairoSVG/2.8.2;paddleocr/3.3.2;paddlepaddle/3.2.2")
	c.VisionModel = env("DRIVE_VISION_MODEL", "")
	c.VisionProvider = env("DRIVE_VISION_PROVIDER_ID", "unconfigured")
	c.OCRModelManifest = env("DRIVE_OCR_MODEL_MANIFEST_SHA256", "e01cdb67ad05659f292bd6cefeadf6c953bbf799ef39e545133738147c59f750")
	// Diagnostic tus logging is metadata only (upload id, method, status, code) and off by default.
	c.TusDiagnostic = os.Getenv("DRIVE_TUS_DIAGNOSTIC") == "1"
	var e error
	c.MeiliKey, e = Secret("DRIVE_MEILI_API_KEY")
	if e != nil {
		return c, e
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DRIVE_DATABASE_URL is required")
	}
	if c.StorageMode != "s3" && c.StorageMode != "local" {
		return c, errors.New("DRIVE_STORAGE_MODE must be local or s3")
	}
	return c, nil
}
