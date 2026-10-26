package delegation

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type Service struct{ d core.Deps }

func New(d core.Deps) *Service { return &Service{d: d} }

type grantVault interface {
	StoreGrant(context.Context, ports.Subject, string, []string) error
	HasGrant(context.Context, ports.Subject) (bool, error)
	Revoke(context.Context, ports.Subject) error
}

func sealSecret(key, plain, ad []byte) ([]byte, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	n := make([]byte, g.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return g.Seal(n, n, plain, ad), nil
}
func openSecret(key, data, ad []byte) ([]byte, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	if len(data) < g.NonceSize() {
		return nil, errors.New("invalid sealed value")
	}
	return g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], ad)
}
func (s *Service) Status(ctx context.Context, a core.Actor) (any, error) {
	v, ok := s.d.Identity.(grantVault)
	if !ok {
		return core.Object{"enabled": false}, nil
	}
	enabled, e := v.HasGrant(ctx, a.Subject)
	return core.Object{"enabled": enabled}, core.NormalizeError(e)
}
func (s *Service) Begin(ctx context.Context, a core.Actor) (core.Object, error) {
	if a.Principal.Type != domain.ActorUser || s.d.Delegation == nil {
		return nil, domain.NewError(domain.AccessDenied, "interactive consent required")
	}
	state, sh, e := core.Token()
	if e != nil {
		return nil, e
	}
	verifier, _, e := core.Token()
	if e != nil {
		return nil, e
	}
	encrypted, e := sealSecret(s.d.Delegation.Key, []byte(verifier), []byte(a.Principal.ID))
	if e != nil {
		return nil, e
	}
	_, e = s.d.DB.Exec(ctx, `INSERT INTO delegation_authorizations(state_hash,principal_id,encrypted_verifier,expires_at) VALUES($1,$2,$3,now()+interval '10 minutes')`, sh, a.Principal.ID, encrypted)
	if e != nil {
		return nil, e
	}
	challenge := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {s.d.Delegation.ClientID}, "redirect_uri": {s.d.Delegation.CallbackURL}, "response_type": {"code"}, "scope": {"openid offline_access drive.read"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "prompt": {"consent"}, "resource": {s.d.Config.MCPURL}}
	return core.Object{"authorization_url": s.d.Delegation.AuthorizationURL + "?" + params.Encode(), "state_cookie": state}, nil
}
func (s *Service) Complete(ctx context.Context, state, code string) error {
	if s.d.Delegation == nil || state == "" || code == "" {
		return domain.NewError(domain.InvalidArgument, "missing authorization callback")
	}
	v, ok := s.d.Identity.(grantVault)
	if !ok {
		return core.Dependency()
	}
	var pid, issuer, subject string
	var encrypted []byte
	e := s.d.DB.QueryRow(ctx, `DELETE FROM delegation_authorizations d USING principals p WHERE d.state_hash=$1 AND d.principal_id=p.id AND d.expires_at>now() AND p.status='active' RETURNING p.id::text,p.issuer,p.subject,d.encrypted_verifier`, core.Digest(state)).Scan(&pid, &issuer, &subject, &encrypted)
	if e != nil {
		return domain.NewError(domain.Unauthenticated, "authorization state expired or already consumed")
	}
	verifier, e := openSecret(s.d.Delegation.Key, encrypted, []byte(pid))
	if e != nil {
		return core.Dependency()
	}
	params := url.Values{"grant_type": {"authorization_code"}, "client_id": {s.d.Delegation.ClientID}, "client_secret": {s.d.Delegation.ClientSecret}, "redirect_uri": {s.d.Delegation.CallbackURL}, "code": {code}, "code_verifier": {string(verifier)}}
	r, e := http.NewRequestWithContext(ctx, "POST", s.d.Delegation.TokenURL, strings.NewReader(params.Encode()))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 30 * time.Second}
	if s.d.Delegation.HTTP != nil {
		*client = *s.d.Delegation.HTTP
		if client.Timeout <= 0 {
			client.Timeout = 30 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(r)
	if e != nil {
		return core.Dependency()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return domain.NewError(domain.AccessDenied, "offline consent could not be exchanged")
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); e != nil {
		return core.Dependency()
	}
	if result.RefreshToken == "" || !strings.Contains(" "+result.Scope+" ", " offline_access ") {
		return domain.NewError(domain.AccessDenied, "explicit offline access was not granted")
	}
	sub, e := s.d.Identity.ValidateToken(ctx, result.AccessToken, ports.TokenRequirements{Resource: s.d.Config.MCPURL, Scopes: []string{"drive.read"}})
	if e != nil || sub.Issuer != issuer || sub.Subject != subject {
		return domain.NewError(domain.AccessDenied, "consent identity did not match the signed-in owner")
	}
	return v.StoreGrant(ctx, sub, result.RefreshToken, strings.Fields(result.Scope))
}
