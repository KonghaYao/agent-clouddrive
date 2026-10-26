// Package identity implements strict RS256 OIDC verification and live Keycloak
// account/group checks. It never treats a JWT as evidence an account is enabled.
package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Issuer, Audience, Resource, JWKSURL, AdminURL, TokenURL                                    string
	ServiceClientID, ServiceClientSecret, ExchangeClientID, ExchangeClientSecret, OCISAudience string
	DelegationVerified                                                                         bool
	Client                                                                                     *http.Client
	ClockSkew                                                                                  time.Duration
	Now                                                                                        func() time.Time
}
type ServiceCredentials struct {
	BackgroundClientID     string `json:"background_client_id"`
	BackgroundClientSecret string `json:"background_client_secret"`
	ClientID               string `json:"client_id"`
	ClientSecret           string `json:"client_secret"`
	ExchangeClientID       string `json:"exchange_client_id"`
	ExchangeClientSecret   string `json:"exchange_client_secret"`
}

func LoadServiceCredentials(file string) (ServiceCredentials, error) {
	var out ServiceCredentials
	f, e := os.Open(file)
	if e != nil {
		return out, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if e != nil || len(b) > 64<<10 {
		return out, ports.ErrInvalid
	}
	if json.Unmarshal(b, &out) != nil || out.ClientID == "" || out.ClientSecret == "" {
		return out, ports.ErrInvalid
	}
	return out, nil
}

type Keycloak struct {
	cfg    Config
	client *http.Client
}

func validURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func NewKeycloak(cfg Config) (*Keycloak, error) {
	if !validURL(cfg.Issuer) || cfg.Audience == "" {
		return nil, ports.ErrInvalid
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = cfg.Issuer + "/protocol/openid-connect/certs"
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = cfg.Issuer + "/protocol/openid-connect/token"
	}
	if cfg.AdminURL == "" {
		parts := strings.Split(cfg.Issuer, "/realms/")
		if len(parts) != 2 || parts[1] == "" || strings.Contains(parts[1], "/") {
			return nil, ports.ErrInvalid
		}
		cfg.AdminURL = parts[0] + "/admin/realms/" + parts[1]
	}
	if !validURL(cfg.JWKSURL) || !validURL(cfg.TokenURL) || !validURL(cfg.AdminURL) {
		return nil, ports.ErrInvalid
	}
	if cfg.ClockSkew < 0 || cfg.ClockSkew > time.Minute {
		return nil, ports.ErrInvalid
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	if cfg.Client != nil {
		*cl = *cfg.Client
		if cl.Timeout == 0 {
			cl.Timeout = 15 * time.Second
		}
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Keycloak{cfg: cfg, client: cl}, nil
}
func (k *Keycloak) request(ctx context.Context, method, endpoint, token string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return ports.ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := k.client.Do(req)
	if e != nil {
		return &ports.DependencyError{Dependency: "identity", Operation: method, Code: "transport_failure", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			cause = ports.ErrUnauthorized
		}
		if resp.StatusCode == 404 {
			cause = ports.ErrNotFound
		}
		return &ports.DependencyError{Dependency: "identity", Operation: method, Code: "http_error", Status: resp.StatusCode, Cause: cause}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if e != nil {
		return ports.ErrUnavailable
	}
	if len(b) > 2<<20 {
		return ports.ErrLimit
	}
	if json.Unmarshal(b, out) != nil {
		return ports.ErrIntegrity
	}
	return nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

func (k *Keycloak) serviceToken(ctx context.Context) (string, error) {
	if k.cfg.ServiceClientID == "" || k.cfg.ServiceClientSecret == "" {
		return "", ports.ErrUnavailable
	}
	var out tokenResponse
	e := k.request(ctx, "POST", k.cfg.TokenURL, "", url.Values{"grant_type": {"client_credentials"}, "client_id": {k.cfg.ServiceClientID}, "client_secret": {k.cfg.ServiceClientSecret}}, &out)
	if e != nil {
		return "", e
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, "Bearer") {
		return "", ports.ErrIntegrity
	}
	return out.AccessToken, nil
}

type claimSet struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	Resource        json.RawMessage `json:"resource"`
	Scope           string          `json:"scope"`
	AuthorizedParty string          `json:"azp"`
	ClientID        string          `json:"client_id"`
	Expires         int64           `json:"exp"`
	NotBefore       int64           `json:"nbf"`
	IssuedAt        int64           `json:"iat"`
}

func stringsClaim(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func safeSubject(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}
func (k *Keycloak) verify(ctx context.Context, raw string, requirements ports.TokenRequirements) (ports.Subject, error) {
	var out ports.Subject
	if len(raw) > 64<<10 {
		return out, ports.ErrUnauthorized
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return out, ports.ErrUnauthorized
	}
	decode := base64.RawURLEncoding.DecodeString
	header, e := decode(parts[0])
	if e != nil {
		return out, ports.ErrUnauthorized
	}
	var hdr struct {
		Algorithm string   `json:"alg"`
		KeyID     string   `json:"kid"`
		Critical  []string `json:"crit"`
	}
	if json.Unmarshal(header, &hdr) != nil || hdr.Algorithm != "RS256" || hdr.KeyID == "" || len(hdr.Critical) > 0 {
		return out, ports.ErrUnauthorized
	}
	payload, e := decode(parts[1])
	if e != nil {
		return out, ports.ErrUnauthorized
	}
	var claims claimSet
	if json.Unmarshal(payload, &claims) != nil {
		return out, ports.ErrUnauthorized
	}
	now := k.cfg.Now()
	skew := int64(k.cfg.ClockSkew.Seconds())
	if claims.Issuer != k.cfg.Issuer || !safeSubject(claims.Subject) || claims.Expires <= 0 || now.Unix() >= claims.Expires+skew || claims.NotBefore > now.Unix()+skew || claims.IssuedAt > now.Unix()+skew || !contains(stringsClaim(claims.Audience), k.cfg.Audience) {
		return out, ports.ErrUnauthorized
	}
	resource := requirements.Resource
	if resource == "" {
		resource = k.cfg.Resource
	}
	if resource != "" && !contains(stringsClaim(claims.Audience), resource) && !contains(stringsClaim(claims.Resource), resource) {
		return out, ports.ErrUnauthorized
	}
	scopes := strings.Fields(claims.Scope)
	for _, s := range requirements.Scopes {
		if !contains(scopes, s) {
			return out, ports.ErrUnauthorized
		}
	}
	var jwks struct {
		Keys []struct {
			Type      string `json:"kty"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
			N         string `json:"n"`
			E         string `json:"e"`
		} `json:"keys"`
	}
	if e = k.request(ctx, "GET", k.cfg.JWKSURL, "", nil, &jwks); e != nil {
		return out, e
	}
	var key *rsa.PublicKey
	for _, j := range jwks.Keys {
		if j.KeyID != hdr.KeyID {
			continue
		}
		if key != nil || j.Type != "RSA" || j.Use != "" && j.Use != "sig" || j.Algorithm != "" && j.Algorithm != "RS256" {
			return out, ports.ErrUnauthorized
		}
		n, e := decode(j.N)
		if e != nil {
			return out, ports.ErrUnauthorized
		}
		ex, e := decode(j.E)
		if e != nil || len(ex) > 4 || len(ex) == 0 {
			return out, ports.ErrUnauthorized
		}
		exponent := 0
		for _, b := range ex {
			exponent = exponent*256 + int(b)
		}
		if exponent < 3 || exponent%2 == 0 {
			return out, ports.ErrUnauthorized
		}
		key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
		if key.N.BitLen() < 2048 {
			return out, ports.ErrUnauthorized
		}
	}
	if key == nil {
		return out, ports.ErrUnauthorized
	}
	signature, e := decode(parts[2])
	if e != nil {
		return out, ports.ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], signature) != nil {
		return out, ports.ErrUnauthorized
	}
	client := claims.AuthorizedParty
	if client == "" {
		client = claims.ClientID
	}
	return ports.Subject{Issuer: claims.Issuer, Subject: claims.Subject, Scopes: scopes, ExpiresAt: time.Unix(claims.Expires, 0).UTC(), ClientID: client}, nil
}
func (k *Keycloak) ValidateToken(ctx context.Context, raw string, requirements ports.TokenRequirements) (ports.Subject, error) {
	subject, e := k.verify(ctx, raw, requirements)
	if e != nil {
		return ports.Subject{}, e
	}
	state, e := k.SubjectStatus(ctx, subject.Issuer, subject.Subject)
	if e != nil {
		return ports.Subject{}, e
	}
	if !state.Enabled {
		// Preserve verified identity only for immediate durable revocation by
		// non-anonymous callers. This remains an authorization failure.
		return subject, errors.Join(ports.ErrUnauthorized, ports.ErrDisabled)
	}
	return subject, nil
}
func (k *Keycloak) SubjectStatus(ctx context.Context, issuer, subject string) (ports.SubjectStatus, error) {
	if issuer != k.cfg.Issuer || !safeSubject(subject) {
		return ports.SubjectStatus{}, ports.ErrUnauthorized
	}
	token, e := k.serviceToken(ctx)
	if e != nil {
		return ports.SubjectStatus{}, e
	}
	var user struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	e = k.request(ctx, "GET", strings.TrimRight(k.cfg.AdminURL, "/")+"/users/"+url.PathEscape(subject), token, nil, &user)
	if errors.Is(e, ports.ErrNotFound) {
		return ports.SubjectStatus{Enabled: false}, nil
	}
	if e != nil {
		return ports.SubjectStatus{}, e
	}
	if user.ID != subject || user.Enabled == nil {
		return ports.SubjectStatus{}, ports.ErrIntegrity
	}
	return ports.SubjectStatus{Enabled: *user.Enabled, NativeUserID: user.ID}, nil
}
func (k *Keycloak) Groups(ctx context.Context, issuer, subject string) ([]string, error) {
	status, e := k.SubjectStatus(ctx, issuer, subject)
	if e != nil {
		return nil, e
	}
	if !status.Enabled {
		return nil, ports.ErrUnauthorized
	}
	token, e := k.serviceToken(ctx)
	if e != nil {
		return nil, e
	}
	out := []string{}
	seen := map[string]bool{}
	for first := 0; first < 10000; first += 100 {
		var page []struct {
			ID string `json:"id"`
		}
		endpoint := strings.TrimRight(k.cfg.AdminURL, "/") + "/users/" + url.PathEscape(subject) + "/groups?first=" + strconv.Itoa(first) + "&max=100"
		if e = k.request(ctx, "GET", endpoint, token, nil, &page); e != nil {
			return nil, e
		}
		for _, g := range page {
			if g.ID == "" {
				return nil, ports.ErrIntegrity
			}
			if !seen[g.ID] {
				out = append(out, g.ID)
				seen[g.ID] = true
			}
		}
		if len(page) < 100 {
			return out, nil
		}
	}
	return nil, ports.ErrLimit
}
func (k *Keycloak) DelegatedCredential(ctx context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if in.Audit != nil {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	if !k.cfg.DelegationVerified || in.SubjectToken == "" || k.cfg.ExchangeClientID == "" || k.cfg.ExchangeClientSecret == "" {
		return ports.Credential{}, ports.ErrUnverified
	}
	if in.Audience == "" || in.Audience != k.cfg.OCISAudience {
		return ports.Credential{}, ports.ErrInvalid
	}
	subject, e := k.ValidateToken(ctx, in.SubjectToken, ports.TokenRequirements{Scopes: in.Scopes})
	if e != nil {
		return ports.Credential{}, e
	}
	if subject.Subject != in.Subject.Subject || subject.Issuer != in.Subject.Issuer {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	var out tokenResponse
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "client_id": {k.cfg.ExchangeClientID}, "client_secret": {k.cfg.ExchangeClientSecret}, "subject_token": {in.SubjectToken}, "subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"}, "requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"}, "audience": {in.Audience}}
	// oCIS calls userinfo: Keycloak exchange must retain openid even when
	// the business caller requested no additional scope.
	exchangeScopes := []string{"openid"}
	for _, scope := range in.Scopes {
		if scope != "openid" {
			exchangeScopes = append(exchangeScopes, scope)
		}
	}
	form.Set("scope", strings.Join(exchangeScopes, " "))
	if e = k.request(ctx, "POST", k.cfg.TokenURL, "", form, &out); e != nil {
		return ports.Credential{}, e
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, "Bearer") || out.ExpiresIn <= 0 {
		return ports.Credential{}, ports.ErrIntegrity
	}
	// Verify the returned token independently; an HTTP 200 cannot authorize a
	// token with the wrong audience, issuer or subject.
	verifier := *k
	verifier.cfg = k.cfg
	verifier.cfg.Audience = in.Audience
	verifier.cfg.Resource = ""
	exchanged, e := verifier.verify(ctx, out.AccessToken, ports.TokenRequirements{})
	if e != nil {
		return ports.Credential{}, e
	}
	if exchanged.Subject != subject.Subject || exchanged.Issuer != subject.Issuer {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	expires := k.cfg.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	if exchanged.ExpiresAt.Before(expires) {
		expires = exchanged.ExpiresAt
	}
	return ports.Credential{AccessToken: out.AccessToken, ExpiresAt: expires}, nil
}

var _ ports.IdentityAdapter = (*Keycloak)(nil)
