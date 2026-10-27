package ocis

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// ServiceCredentialConfig configures a service credential provider. The default
// instance is read-only for background jobs; AllowWrite=true is a separate,
// independently verified write-capable instance. Neither is accepted as an
// inbound bearer token.
type ServiceCredentialConfig struct {
	TokenURL, ClientID, ClientSecret, Audience string
	Scopes                                     []string
	TTL                                        time.Duration
	Verified                                   bool
	// AllowWrite enables the separate write-capable instance. The read-only
	// instance must keep this false.
	AllowWrite bool
	Client     *http.Client
	Now        func() time.Time
}

type ServiceCredential struct {
	cfg    ServiceCredentialConfig
	client *http.Client
}

func NewServiceCredential(cfg ServiceCredentialConfig) (*ServiceCredential, error) {
	u, e := url.Parse(cfg.TokenURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Audience == "" {
		return nil, ports.ErrInvalid
	}
	if cfg.AllowWrite {
		if !hasScope(cfg.Scopes, "drive.write") {
			return nil, ports.ErrInvalid
		}
		for _, scope := range cfg.Scopes {
			if scope != "drive.read" && scope != "drive.write" && scope != "openid" {
				return nil, ports.ErrInvalid
			}
		}
		cfg.Scopes = writeScopes(cfg.Scopes)
	} else {
		if len(cfg.Scopes) == 0 {
			cfg.Scopes = []string{"drive.read"}
		}
		for _, scope := range cfg.Scopes {
			// Background native access is read-only; a write-capable service token
			// would be an unbounded escalation path.
			if scope != "drive.read" {
				return nil, ports.ErrInvalid
			}
		}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	if cfg.Client != nil {
		*cl = *cfg.Client
		if cl.Timeout == 0 {
			cl.Timeout = 30 * time.Second
		}
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ServiceCredential{cfg: cfg, client: cl}, nil
}

// Verified reports whether this independent provider is allowed to issue
// credentials. Callers use it as defense-in-depth before a network call.
func (p *ServiceCredential) Verified() bool { return p.cfg.Verified }

func (p *ServiceCredential) JobCredential(ctx context.Context, req ports.JobCredentialRequest) (ports.Credential, error) {
	if !p.cfg.Verified {
		return ports.Credential{}, ports.ErrUnverified
	}
	if req.Audience != p.cfg.Audience {
		return ports.Credential{}, ports.ErrInvalid
	}
	if p.cfg.AllowWrite {
		if !hasScope(req.Scopes, "drive.write") {
			return ports.Credential{}, ports.ErrUnauthorized
		}
		for _, scope := range req.Scopes {
			if scope != "drive.read" && scope != "drive.write" && scope != "openid" {
				return ports.Credential{}, ports.ErrUnauthorized
			}
		}
	} else {
		for _, scope := range req.Scopes {
			if scope != "drive.read" {
				return ports.Credential{}, ports.ErrUnauthorized
			}
		}
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
		"audience":      {p.cfg.Audience},
		"scope":         {strings.Join(p.cfg.Scopes, " ")},
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.TokenURL, strings.NewReader(form.Encode()))
	if e != nil {
		return ports.Credential{}, ports.ErrInvalid
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := p.client.Do(r)
	if e != nil {
		return ports.Credential{}, &ports.DependencyError{Dependency: "ocis", Operation: "service_credential", Code: "transport_failure", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cause = ports.ErrUnauthorized
		}
		// Never include the response body or token in the error.
		return ports.Credential{}, &ports.DependencyError{Dependency: "ocis", Operation: "service_credential", Status: resp.StatusCode, Code: "token_rejected", Cause: cause}
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(body) > 1<<20 {
		return ports.Credential{}, ports.ErrIntegrity
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" || (out.TokenType != "" && !strings.EqualFold(out.TokenType, "Bearer")) {
		return ports.Credential{}, ports.ErrIntegrity
	}
	ttl := p.cfg.TTL
	if out.ExpiresIn > 0 && out.ExpiresIn <= int64(p.cfg.TTL/time.Second) {
		ttl = time.Duration(out.ExpiresIn) * time.Second
	}
	return ports.Credential{AccessToken: out.AccessToken, ExpiresAt: p.cfg.Now().Add(ttl)}, nil
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func writeScopes(in []string) []string {
	// A write token must also support pre-mutation stat calls.
	out := []string{"drive.read", "drive.write"}
	seen := map[string]bool{"drive.read": true, "drive.write": true}
	for _, scope := range in {
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	return out
}

var _ ports.JobCredentialProvider = (*ServiceCredential)(nil)
