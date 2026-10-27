// Package config loads a fail-closed deployment configuration. No credentials are logged.
package config

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
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

	// MasterKeys authenticates a trusted caller that asserts a subject through
	// X-On-Behalf-Of. Empty means the master-key path is disabled (fail closed).
	MasterKeys                   map[string]string
	MasterKeyDefaultID           string
	MasterKeyIssuer              string
	MasterKeyInteractiveClientID string
	MasterKeyAllowedCIDRs        []string
	MasterKeyStatusURL           string
	MasterKeyStatusToken         string
	MasterKeyStatusTTL           time.Duration
	MasterKeyScopes              []string

	// Background job credentials never authenticate inbound requests.
	JobCredentialMode       string
	OCISServiceTokenURL     string
	OCISServiceClientID     string
	OCISServiceClientSecret string
	OCISServiceAudience     string
	OCISServiceScopes       []string
	JobCredentialTTL        time.Duration

	// External write credentials are a separate, independently verified
	// capability. They are never used for background jobs or inbound auth.
	ExternalWriteEnabled         bool
	ExternalWriteIssuers         []string
	OCISWriteServiceTokenURL     string
	OCISWriteServiceClientID     string
	OCISWriteServiceClientSecret string
	OCISWriteServiceAudience     string
	OCISWriteServiceScopes       []string
	OCISWriteServiceTTL          time.Duration
	OCISWriteServiceVerified     bool

	// The space provisioner creates native project spaces for externally
	// asserted identities. It is a separate high-privilege client.
	ExternalSpaceIssuers           []string
	SpaceProvisionerEnabled        bool
	SpaceProvisionerTokenURL       string
	SpaceProvisionerClientID       string
	SpaceProvisionerClientSecret   string
	SpaceProvisionerAudience       string
	SpaceProvisionerScopes         []string
	SpaceProvisionerVerified       bool
	SpaceProvisionerNativeInstance string
}

const (
	// These are the optional drive scopes configured on the interactive web
	// client in deploy/compose/keycloak/clouddrive-realm.template.json.
	defaultDriveScopes           = "drive.read,drive.write,drive.delete,drive.restore,drive.convert,drive.share.request,drive.feedback"
	defaultServiceScope          = "drive.read"
	defaultWriteServiceScopes    = "drive.read,drive.write"
	defaultSpaceProvisionerScope = "openid"
)

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
func list(k, d string) []string {
	out := []string{}
	for _, v := range strings.Split(env(k, d), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func scopeList(k, d string) []string {
	out := []string{}
	for _, v := range strings.FieldsFunc(env(k, d), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func positive(v, d int64) int64 {
	if v <= 0 {
		return d
	}
	return v
}
func httpBaseURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func parseMasterKeys() (map[string]string, error) {
	raw, e := Secret("DRIVE_MASTER_KEYS")
	if e != nil {
		return nil, e
	}
	if raw == "" {
		return nil, nil
	}
	var keys map[string]string
	if json.Unmarshal([]byte(raw), &keys) != nil {
		return nil, errors.New("DRIVE_MASTER_KEYS must be a JSON object of key id to secret")
	}
	for id, secret := range keys {
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "\x00\r\n\t") || secret == "" || len(secret) > 4096 {
			return nil, errors.New("DRIVE_MASTER_KEYS contains an empty, overlong or unsafe entry")
		}
	}
	return keys, nil
}
func Load() (Config, error) {
	c := Config{DatabaseURL: env("DRIVE_DATABASE_URL", ""), NATSURL: env("DRIVE_NATS_URL", "nats://nats:4222"), NATSStream: env("DRIVE_NATS_STREAM", "CLOUDDRIVE"), Listen: env("DRIVE_LISTEN", ":8080"), MCPListen: env("DRIVE_MCP_LISTEN", ":8081"), PublicURL: env("DRIVE_PUBLIC_URL", "http://localhost:8080"), MCPURL: env("DRIVE_MCP_PUBLIC_URL", "http://localhost:8080/mcp"), OIDCIssuer: env("DRIVE_OIDC_ISSUER", ""), Audience: env("DRIVE_OAUTH_AUDIENCE", "agent-clouddrive"), WebClientID: env("DRIVE_WEB_CLIENT_ID", "clouddrive-web"), OCISURL: env("DRIVE_OCIS_INTERNAL_URL", "http://ocis:9200"), OCISAudience: env("DRIVE_OCIS_AUDIENCE", "ocis"), TrashDir: env("DRIVE_TRASH_RESERVED_DIR", ".clouddrive-retained"), ArtifactRoot: env("DRIVE_ARTIFACT_LOCAL_ROOT", "/data/artifacts"), StorageMode: env("DRIVE_STORAGE_MODE", "s3"), MeiliURL: env("DRIVE_MEILI_URL", "http://meilisearch:7700"), MeiliIndex: env("DRIVE_MEILI_INDEX", "clouddrive"), AnyDocURL: env("DRIVE_ANYDOC_URL", "http://anydoc:8090"), OCRURL: env("DRIVE_OCR_URL", "http://paddleocr:8090"), PreviewURL: env("DRIVE_PREVIEW_INTERNAL_URL", "http://preview:8090"), NodeID: env("DRIVE_NODE_ID", "single-node"), AllowedOrigins: strings.Split(env("DRIVE_ALLOWED_ORIGINS", "http://localhost:8080"), ","), UploadMax: integer("DRIVE_UPLOAD_MAX_BYTES", 20<<30), ConvertMax: integer("DRIVE_CONVERT_MAX_BYTES", 100<<20), DefaultQuota: integer("DRIVE_DEFAULT_QUOTA_BYTES", 100<<30), ConvertPages: int(integer("DRIVE_CONVERT_MAX_PAGES", 500)), Lease: time.Duration(integer("DRIVE_JOB_LEASE_SECONDS", 300)) * time.Second, AllowUnverifiedOCIS: os.Getenv("DRIVE_OCIS_INTEGRATION_VERIFIED") == "true"}
	c.PipelineVersion = env("DRIVE_CONVERTER_PIPELINE_VERSION", "clouddrive-converter/1.0.0;anydoc/0.2.4;pypdf/6.10.0;pypdfium2/5.14.0;Pillow/12.3.0;CairoSVG/2.8.2;paddleocr/3.3.2;paddlepaddle/3.2.2")
	c.VisionModel = env("DRIVE_VISION_MODEL", "")
	c.VisionProvider = env("DRIVE_VISION_PROVIDER_ID", "unconfigured")
	c.OCRModelManifest = env("DRIVE_OCR_MODEL_MANIFEST_SHA256", "e01cdb67ad05659f292bd6cefeadf6c953bbf799ef39e545133738147c59f750")
	var e error
	c.MasterKeys, e = parseMasterKeys()
	if e != nil {
		return c, e
	}
	c.MasterKeyDefaultID = env("DRIVE_MASTER_KEY_DEFAULT_ID", "")
	c.MasterKeyIssuer = env("DRIVE_MASTER_KEY_ISSUER", "fenix")
	c.MasterKeyInteractiveClientID = env("DRIVE_MASTER_KEY_INTERACTIVE_CLIENT_ID", c.WebClientID)
	c.MasterKeyAllowedCIDRs = list("DRIVE_MASTER_KEY_ALLOWED_CIDRS", "")
	c.MasterKeyStatusURL = strings.TrimRight(env("DRIVE_MASTER_KEY_STATUS_URL", ""), "/")
	c.MasterKeyStatusTTL = time.Duration(positive(integer("DRIVE_MASTER_KEY_STATUS_TTL_SECONDS", 60), 60)) * time.Second
	c.MasterKeyScopes = scopeList("DRIVE_MASTER_KEY_SCOPES", defaultDriveScopes)
	if c.MasterKeyDefaultID != "" && len(c.MasterKeys) > 0 {
		if _, ok := c.MasterKeys[c.MasterKeyDefaultID]; !ok {
			return c, errors.New("DRIVE_MASTER_KEY_DEFAULT_ID is not present in DRIVE_MASTER_KEYS")
		}
	}
	for _, cidr := range c.MasterKeyAllowedCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return c, errors.New("DRIVE_MASTER_KEY_ALLOWED_CIDRS contains an invalid CIDR")
		}
	}
	if c.MasterKeyStatusURL != "" && !httpBaseURL(c.MasterKeyStatusURL) {
		return c, errors.New("DRIVE_MASTER_KEY_STATUS_URL must be an http(s) base URL without credentials, query or fragment")
	}
	c.JobCredentialMode = env("DRIVE_JOB_CREDENTIAL_MODE", "auto")
	switch c.JobCredentialMode {
	case "auto", "delegation", "service", "disabled":
	default:
		return c, errors.New("DRIVE_JOB_CREDENTIAL_MODE must be auto, delegation, service or disabled")
	}
	c.OCISServiceTokenURL = strings.TrimRight(env("DRIVE_OCIS_SERVICE_TOKEN_URL", ""), "/")
	c.OCISServiceClientID = env("DRIVE_OCIS_SERVICE_CLIENT_ID", "")
	c.OCISServiceClientSecret, e = Secret("DRIVE_OCIS_SERVICE_CLIENT_SECRET")
	if e != nil {
		return c, e
	}
	c.OCISServiceAudience = env("DRIVE_OCIS_SERVICE_AUDIENCE", "ocis")
	c.OCISServiceScopes = scopeList("DRIVE_OCIS_SERVICE_SCOPES", defaultServiceScope)
	c.JobCredentialTTL = time.Duration(positive(integer("DRIVE_JOB_CREDENTIAL_TTL_SECONDS", 300), 300)) * time.Second
	if c.OCISServiceTokenURL != "" || c.OCISServiceClientID != "" || c.OCISServiceClientSecret != "" {
		if c.OCISServiceTokenURL == "" || c.OCISServiceClientID == "" || c.OCISServiceClientSecret == "" || c.OCISServiceAudience == "" {
			return c, errors.New("DRIVE_OCIS_SERVICE_TOKEN_URL, DRIVE_OCIS_SERVICE_CLIENT_ID and DRIVE_OCIS_SERVICE_CLIENT_SECRET must be configured together")
		}
		if !httpBaseURL(c.OCISServiceTokenURL) {
			return c, errors.New("DRIVE_OCIS_SERVICE_TOKEN_URL must be an http(s) URL without credentials, query or fragment")
		}
		for _, scope := range c.OCISServiceScopes {
			if scope != defaultServiceScope {
				return c, errors.New("DRIVE_OCIS_SERVICE_SCOPES must be read-only (drive.read)")
			}
		}
	}
	c.ExternalWriteEnabled = os.Getenv("DRIVE_EXTERNAL_WRITE_ENABLED") == "true"
	c.ExternalWriteIssuers = list("DRIVE_EXTERNAL_WRITE_ISSUERS", "fenix")
	c.OCISWriteServiceTokenURL = strings.TrimRight(env("DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL", ""), "/")
	c.OCISWriteServiceClientID = env("DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID", "")
	c.OCISWriteServiceClientSecret, e = Secret("DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET")
	if e != nil {
		return c, e
	}
	c.OCISWriteServiceAudience = env("DRIVE_OCIS_WRITE_SERVICE_AUDIENCE", "ocis")
	c.OCISWriteServiceScopes = scopeList("DRIVE_OCIS_WRITE_SERVICE_SCOPES", defaultWriteServiceScopes)
	c.OCISWriteServiceTTL = time.Duration(positive(integer("DRIVE_OCIS_WRITE_SERVICE_TTL_SECONDS", 300), 300)) * time.Second
	c.OCISWriteServiceVerified = os.Getenv("DRIVE_OCIS_WRITE_SERVICE_VERIFIED") == "true"
	writeConfigured := c.OCISWriteServiceTokenURL != "" || c.OCISWriteServiceClientID != "" || c.OCISWriteServiceClientSecret != ""
	if writeConfigured {
		if c.OCISWriteServiceTokenURL == "" || c.OCISWriteServiceClientID == "" || c.OCISWriteServiceClientSecret == "" || c.OCISWriteServiceAudience == "" {
			return c, errors.New("DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL, DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID and DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET must be configured together")
		}
		if !httpBaseURL(c.OCISWriteServiceTokenURL) {
			return c, errors.New("DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL must be an http(s) URL without credentials, query or fragment")
		}
		hasWrite := false
		for _, scope := range c.OCISWriteServiceScopes {
			if scope == "drive.write" {
				hasWrite = true
			}
			if scope != "drive.read" && scope != "drive.write" && scope != "openid" {
				return c, errors.New("DRIVE_OCIS_WRITE_SERVICE_SCOPES may contain only drive.read, drive.write or openid")
			}
		}
		if !hasWrite {
			return c, errors.New("DRIVE_OCIS_WRITE_SERVICE_SCOPES must contain drive.write")
		}
	}
	if c.ExternalWriteEnabled {
		if len(c.ExternalWriteIssuers) == 0 {
			return c, errors.New("DRIVE_EXTERNAL_WRITE_ISSUERS must not be empty when DRIVE_EXTERNAL_WRITE_ENABLED=true")
		}
		if !writeConfigured {
			return c, errors.New("DRIVE_EXTERNAL_WRITE_ENABLED requires a configured write service credential")
		}
		if !c.OCISWriteServiceVerified {
			return c, errors.New("DRIVE_EXTERNAL_WRITE_ENABLED requires DRIVE_OCIS_WRITE_SERVICE_VERIFIED=true")
		}
	}
	c.ExternalSpaceIssuers = list("DRIVE_EXTERNAL_SPACE_ISSUERS", "fenix")
	c.SpaceProvisionerEnabled = os.Getenv("DRIVE_SPACE_PROVISIONER_ENABLED") == "true"
	c.SpaceProvisionerTokenURL = strings.TrimRight(env("DRIVE_SPACE_PROVISIONER_TOKEN_URL", ""), "/")
	c.SpaceProvisionerClientID = env("DRIVE_SPACE_PROVISIONER_CLIENT_ID", "")
	c.SpaceProvisionerClientSecret, e = Secret("DRIVE_SPACE_PROVISIONER_CLIENT_SECRET")
	if e != nil {
		return c, e
	}
	c.SpaceProvisionerAudience = env("DRIVE_SPACE_PROVISIONER_AUDIENCE", "ocis")
	c.SpaceProvisionerScopes = scopeList("DRIVE_SPACE_PROVISIONER_SCOPES", defaultSpaceProvisionerScope)
	c.SpaceProvisionerVerified = os.Getenv("DRIVE_SPACE_PROVISIONER_VERIFIED") == "true"
	c.SpaceProvisionerNativeInstance = strings.TrimSpace(env("DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE", "ocis"))
	if c.SpaceProvisionerNativeInstance == "" || len(c.SpaceProvisionerNativeInstance) > 128 || strings.ContainsAny(c.SpaceProvisionerNativeInstance, " \t\r\n\x00") {
		return c, errors.New("DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE must be a non-empty instance name")
	}
	provisionerConfigured := c.SpaceProvisionerTokenURL != "" || c.SpaceProvisionerClientID != "" || c.SpaceProvisionerClientSecret != ""
	if provisionerConfigured {
		if c.SpaceProvisionerTokenURL == "" || c.SpaceProvisionerClientID == "" || c.SpaceProvisionerClientSecret == "" || c.SpaceProvisionerAudience == "" {
			return c, errors.New("DRIVE_SPACE_PROVISIONER_TOKEN_URL, DRIVE_SPACE_PROVISIONER_CLIENT_ID and DRIVE_SPACE_PROVISIONER_CLIENT_SECRET must be configured together")
		}
		if !httpBaseURL(c.SpaceProvisionerTokenURL) {
			return c, errors.New("DRIVE_SPACE_PROVISIONER_TOKEN_URL must be an http(s) URL without credentials, query or fragment")
		}
		if len(c.SpaceProvisionerScopes) == 0 {
			return c, errors.New("DRIVE_SPACE_PROVISIONER_SCOPES must be openid")
		}
		for _, scope := range c.SpaceProvisionerScopes {
			if scope != defaultSpaceProvisionerScope {
				return c, errors.New("DRIVE_SPACE_PROVISIONER_SCOPES must be openid")
			}
		}
	}
	if c.SpaceProvisionerEnabled {
		if len(c.ExternalSpaceIssuers) == 0 {
			return c, errors.New("DRIVE_EXTERNAL_SPACE_ISSUERS must not be empty when DRIVE_SPACE_PROVISIONER_ENABLED=true")
		}
		if !provisionerConfigured {
			return c, errors.New("DRIVE_SPACE_PROVISIONER_ENABLED requires a configured provisioner client")
		}
		if !c.SpaceProvisionerVerified {
			return c, errors.New("DRIVE_SPACE_PROVISIONER_ENABLED requires DRIVE_SPACE_PROVISIONER_VERIFIED=true")
		}
	}
	if c.MasterKeyStatusToken, e = Secret("DRIVE_MASTER_KEY_STATUS_TOKEN"); e != nil {
		return c, e
	}
	// Diagnostic tus logging is metadata only (upload id, method, status, code) and off by default.
	c.TusDiagnostic = os.Getenv("DRIVE_TUS_DIAGNOSTIC") == "1"
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
