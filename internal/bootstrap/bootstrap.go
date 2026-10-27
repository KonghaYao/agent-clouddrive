package bootstrap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/artifactstore"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/converters"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/delegation"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/identity"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/ocis"
	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/search"
	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"os"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func Open(ctx context.Context) (*application.Service, func(), error) {
	cfg, e := config.Load()
	if e != nil {
		return nil, nil, e
	}
	pool, e := pgxpool.New(ctx, cfg.DatabaseURL)
	if e != nil {
		return nil, nil, e
	}
	closeFn := func() { pool.Close() }
	if e = pool.Ping(ctx); e != nil {
		closeFn()
		return nil, nil, e
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p := os.Getenv("DRIVE_CA_CERT_FILE"); p != "" {
		pem, e := os.ReadFile(p)
		if e != nil {
			closeFn()
			return nil, nil, e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			closeFn()
			return nil, nil, errors.New("invalid DRIVE_CA_CERT_FILE")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Transport: transport, Timeout: 0}
	original, e := ocis.New(ocis.Config{URL: cfg.OCISURL, Instance: cfg.OCISURL, InstanceAliases: []string{cfg.SpaceProvisionerNativeInstance}, Client: client, MaxUploadChunkBytes: 100 << 20, VerifiedRevisionRead: cfg.AllowUnverifiedOCIS, VerifiedMoveHistory: cfg.AllowUnverifiedOCIS, VerifiedConditionalOverwrite: cfg.AllowUnverifiedOCIS})
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	creds, e := identity.LoadServiceCredentials(os.Getenv("DRIVE_IDENTITY_SERVICE_CREDENTIAL_FILE"))
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	var jobProvider ports.JobCredentialProvider
	if cfg.OCISServiceTokenURL != "" {
		provider, er := ocis.NewServiceCredential(ocis.ServiceCredentialConfig{TokenURL: cfg.OCISServiceTokenURL, ClientID: cfg.OCISServiceClientID, ClientSecret: cfg.OCISServiceClientSecret, Audience: cfg.OCISServiceAudience, Scopes: cfg.OCISServiceScopes, TTL: cfg.JobCredentialTTL, Verified: cfg.AllowUnverifiedOCIS && os.Getenv("DRIVE_DELEGATION_VERIFIED") == "true", Client: client})
		if er != nil {
			closeFn()
			return nil, nil, er
		}
		jobProvider = provider
	}
	var writeProvider ports.JobCredentialProvider
	if cfg.OCISWriteServiceTokenURL != "" {
		provider, er := ocis.NewServiceCredential(ocis.ServiceCredentialConfig{TokenURL: cfg.OCISWriteServiceTokenURL, ClientID: cfg.OCISWriteServiceClientID, ClientSecret: cfg.OCISWriteServiceClientSecret, Audience: cfg.OCISWriteServiceAudience, Scopes: cfg.OCISWriteServiceScopes, TTL: cfg.OCISWriteServiceTTL, Verified: cfg.OCISWriteServiceVerified, AllowWrite: true, Client: client})
		if er != nil {
			closeFn()
			return nil, nil, er
		}
		writeProvider = provider
	}
	var spaceProvisioner ports.SpaceProvisioner
	if cfg.SpaceProvisionerEnabled {
		provider, er := ocis.NewSpaceProvisioner(ocis.SpaceProvisionerConfig{URL: cfg.OCISURL, Instance: cfg.SpaceProvisionerNativeInstance, TokenURL: cfg.SpaceProvisionerTokenURL, ClientID: cfg.SpaceProvisionerClientID, ClientSecret: cfg.SpaceProvisionerClientSecret, Audience: cfg.SpaceProvisionerAudience, Scopes: cfg.SpaceProvisionerScopes, Verified: cfg.SpaceProvisionerVerified, Client: client})
		if er != nil {
			closeFn()
			return nil, nil, er
		}
		spaceProvisioner = provider
	}
	externalWriteIssuers := map[string]bool{}
	for _, issuer := range cfg.ExternalWriteIssuers {
		externalWriteIssuers[issuer] = true
	}
	if cfg.ExternalWriteEnabled && (writeProvider == nil || len(externalWriteIssuers) == 0) {
		closeFn()
		return nil, nil, errors.New("external writes enabled without a configured write service credential or allowed issuer")
	}
	ident, e := identity.NewKeycloak(identity.Config{Issuer: cfg.OIDCIssuer, Audience: cfg.Audience, Resource: cfg.MCPURL, JWKSURL: os.Getenv("DRIVE_OIDC_JWKS_URL"), AdminURL: os.Getenv("DRIVE_KEYCLOAK_ADMIN_URL"), TokenURL: os.Getenv("DRIVE_OIDC_TOKEN_URL"), ServiceClientID: creds.ClientID, ServiceClientSecret: creds.ClientSecret, ExchangeClientID: creds.ExchangeClientID, ExchangeClientSecret: creds.ExchangeClientSecret, OCISAudience: cfg.OCISAudience, DelegationVerified: os.Getenv("DRIVE_DELEGATION_VERIFIED") == "true", Client: client})
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	var artifacts ports.ArtifactStore
	if cfg.StorageMode == "local" {
		artifacts, e = artifactstore.NewLocal(cfg.ArtifactRoot, 128<<20)
	} else {
		ak, er := config.Secret("DRIVE_S3_ACCESS_KEY")
		if er != nil {
			closeFn()
			return nil, nil, er
		}
		sk, er := config.Secret("DRIVE_S3_SECRET_KEY")
		if er != nil {
			closeFn()
			return nil, nil, er
		}
		artifacts, e = artifactstore.NewS3(artifactstore.S3Config{Endpoint: os.Getenv("DRIVE_S3_ENDPOINT"), Region: env("DRIVE_S3_REGION", "us-east-1"), Bucket: env("DRIVE_S3_ARTIFACT_BUCKET", "clouddrive-artifacts"), AccessKey: ak, SecretKey: sk, ForcePathStyle: env("DRIVE_S3_FORCE_PATH_STYLE", "true") == "true", MaxObjectBytes: 128 << 20, Client: client})
	}
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	index, e := search.New(search.Config{URL: cfg.MeiliURL, APIKey: cfg.MeiliKey, Index: cfg.MeiliIndex, Client: client})
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	secret, e := config.Secret("DRIVE_CONVERTER_TOKEN")
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	conv, e := converters.New(converters.Config{BaseURL: cfg.AnyDocURL, BearerToken: secret, MaxResponseBytes: 128 << 20, Timeout: 10 * time.Minute, Client: client})
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	preview, e := converters.New(converters.Config{BaseURL: cfg.PreviewURL, BearerToken: secret, MaxResponseBytes: 128 << 20, Timeout: 10 * time.Minute, Client: client})
	if e != nil {
		closeFn()
		return nil, nil, e
	}
	var identityPort ports.IdentityAdapter = ident
	var delegationConfig *application.DelegationConfig
	if keyFile := os.Getenv("DRIVE_DELEGATION_KEY_FILE"); keyFile != "" {
		key, e := os.ReadFile(keyFile)
		if e != nil {
			closeFn()
			return nil, nil, e
		}
		tokenURL := env("DRIVE_OIDC_TOKEN_URL", cfg.OIDCIssuer+"/protocol/openid-connect/token")
		wrapped, e := delegation.New(delegation.Config{Base: ident, DB: pool, Issuer: cfg.OIDCIssuer, TokenURL: tokenURL, ClientID: creds.BackgroundClientID, ClientSecret: creds.BackgroundClientSecret, Key: key, Client: client, Verified: os.Getenv("DRIVE_DELEGATION_VERIFIED") == "true", JobCredentialMode: cfg.JobCredentialMode, JobCredentialProvider: jobProvider, WriteCredential: writeProvider, ExternalWriteIssuers: externalWriteIssuers, ExternalWriteEnabled: cfg.ExternalWriteEnabled})
		if e != nil {
			closeFn()
			return nil, nil, e
		}
		identityPort = wrapped
		delegationConfig = &application.DelegationConfig{ClientID: creds.BackgroundClientID, ClientSecret: creds.BackgroundClientSecret, AuthorizationURL: cfg.OIDCIssuer + "/protocol/openid-connect/auth", TokenURL: tokenURL, CallbackURL: cfg.PublicURL + "/v1/delegation/callback", Key: key, HTTP: client}
	}
	svc := &application.Service{DB: pool, Config: cfg, Original: original, Artifacts: artifacts, Identity: identityPort, Search: index, Converter: conv, Preview: preview, Delegation: delegationConfig, MasterKeyStatus: core.NewMasterKeyStatusCache(), SpaceProvisioner: spaceProvisioner}
	return svc, closeFn, nil
}
