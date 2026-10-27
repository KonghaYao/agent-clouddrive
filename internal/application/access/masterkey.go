package access

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// ErrInvalidOnBehalfOf marks a malformed trusted-caller assertion instead of an
// identity or authorization decision. Transports map it to HTTP 400.
var ErrInvalidOnBehalfOf = errors.New("invalid X-On-Behalf-Of")

// MasterKeyAuth is a trusted caller's assertion. Bearer is a secret and must
// never be logged or forwarded to a native backend.
type MasterKeyAuth struct {
	Bearer     string
	KeyID      string
	OnBehalfOf string
	RemoteIP   string
	Route      string
	Agent      bool
	ReadOnly   bool
}

var onBehalfOfPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)

func validOnBehalfOf(v string) bool { return onBehalfOfPattern.MatchString(v) }

func auditText(route string) string {
	if runes := []rune(route); len(runes) > 256 {
		route = string(runes[:256])
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, route)
}

var masterKeyClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// IsMasterKey reports whether raw matches any configured key. It only routes a
// request that omitted X-On-Behalf-Of; authentication repeats selection.
func (s *Service) IsMasterKey(raw string) bool {
	if raw == "" || len(s.d.Config.MasterKeys) == 0 {
		return false
	}
	match := 0
	for _, secret := range s.d.Config.MasterKeys {
		match |= subtle.ConstantTimeCompare([]byte(secret), []byte(raw))
	}
	return match == 1
}

// AuthenticateMasterKey authenticates a trusted caller and projects the asserted
// subject through the same principal, quota and ACL path as OIDC identities.
// The presented key never becomes the actor token: it is not a user credential
// and must not reach oCIS.
func (s *Service) AuthenticateMasterKey(ctx context.Context, in MasterKeyAuth) (a core.Actor, err error) {
	keyID, ok := s.selectMasterKey(in.KeyID, in.Bearer)
	auditKeyID := keyID
	if auditKeyID == "" && in.KeyID != "" {
		if _, exists := s.d.Config.MasterKeys[in.KeyID]; exists {
			auditKeyID = in.KeyID
		}
	}
	auditSubject := ""
	if validOnBehalfOf(in.OnBehalfOf) {
		auditSubject = in.OnBehalfOf
	}
	defer func() {
		status := 200
		if err != nil {
			status = masterKeyHTTPStatus(err)
		}
		slog.Info("master key authentication", "credential_source", "master_key", "key_id", auditKeyID, "on_behalf_of", auditSubject, "route", auditText(in.Route), "status", status)
	}()
	if !ok {
		return a, domain.NewError(domain.Unauthenticated, "invalid master key")
	}
	if !validOnBehalfOf(in.OnBehalfOf) {
		return a, ErrInvalidOnBehalfOf
	}
	if err = s.authorizeMasterKeySource(in.RemoteIP); err != nil {
		return a, err
	}
	enabled, e := s.masterKeyStatus(ctx, in.OnBehalfOf)
	if e != nil {
		return a, core.Dependency()
	}
	sub := ports.Subject{Issuer: s.d.Config.MasterKeyIssuer, Subject: in.OnBehalfOf}
	if !enabled {
		s.disableIdentity(ctx, sub, in.ReadOnly)
		return a, domain.NewError(domain.AccessDenied, "account disabled")
	}
	sub.ClientID = s.d.Config.MasterKeyInteractiveClientID
	if sub.ClientID == "" {
		sub.ClientID = s.d.Config.WebClientID
	}
	sub.Scopes = s.d.Config.MasterKeyScopes
	// Token stays empty: a master key only proves the caller, never a user's
	// delegated credential.
	return s.finishAuthentication(ctx, sub, "", in.Agent, in.ReadOnly)
}

func (s *Service) selectMasterKey(requested, presented string) (string, bool) {
	if presented == "" || len(s.d.Config.MasterKeys) == 0 {
		return "", false
	}
	want := requested
	if want == "" {
		want = s.d.Config.MasterKeyDefaultID
	}
	if want == "" {
		return "", false
	}
	var resolved string
	match := 0
	for id, secret := range s.d.Config.MasterKeys {
		eq := subtle.ConstantTimeCompare([]byte(id), []byte(want)) & subtle.ConstantTimeCompare([]byte(secret), []byte(presented))
		if eq == 1 {
			resolved = id
		}
		match |= eq
	}
	if match != 1 {
		return "", false
	}
	return resolved, true
}

func (s *Service) authorizeMasterKeySource(remote string) error {
	denied := domain.NewError(domain.AccessDenied, "master key source is not allowed")
	if len(s.d.Config.MasterKeyAllowedCIDRs) == 0 {
		return denied
	}
	ip := net.ParseIP(strings.TrimSpace(remote))
	if ip == nil {
		return denied
	}
	for _, raw := range s.d.Config.MasterKeyAllowedCIDRs {
		_, network, e := net.ParseCIDR(raw)
		if e == nil && network.Contains(ip) {
			return nil
		}
	}
	return denied
}

func (s *Service) masterKeyStatus(ctx context.Context, subject string) (bool, error) {
	if s.d.Config.MasterKeyStatusURL == "" {
		// No authoritative status endpoint is configured. The trusted caller's
		// assertion is accepted until the local principal is disabled; this is a
		// residual risk documented for deployments that opt out.
		return true, nil
	}
	now := time.Now()
	if cache := s.d.MasterKeyStatus; cache != nil {
		if enabled, ok := cache.Get(subject, now); ok {
			return enabled, nil
		}
	}
	enabled, e := s.fetchMasterKeyStatus(ctx, subject)
	if e != nil {
		return false, e
	}
	if cache := s.d.MasterKeyStatus; cache != nil {
		cache.Put(subject, enabled, now.Add(s.masterKeyStatusTTL()))
	}
	return enabled, nil
}

func (s *Service) masterKeyStatusTTL() time.Duration {
	if ttl := s.d.Config.MasterKeyStatusTTL; ttl > 0 {
		return ttl
	}
	return time.Minute
}

func (s *Service) fetchMasterKeyStatus(ctx context.Context, subject string) (bool, error) {
	endpoint := strings.TrimRight(s.d.Config.MasterKeyStatusURL, "/") + "/" + url.PathEscape(subject) + "/status"
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return false, ports.ErrInvalid
	}
	req.Header.Set("Accept", "application/json")
	if token := s.d.Config.MasterKeyStatusToken; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := masterKeyClient.Do(req)
	if e != nil {
		return false, &ports.DependencyError{Dependency: "identity_status", Operation: "GET", Code: "transport_failure", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, &ports.DependencyError{Dependency: "identity_status", Operation: "GET", Status: resp.StatusCode, Code: "status_rejected", Cause: ports.ErrUnavailable}
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if e != nil || len(body) > 64<<10 {
		return false, ports.ErrIntegrity
	}
	var out struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(body, &out) != nil || out.Enabled == nil {
		return false, ports.ErrIntegrity
	}
	return *out.Enabled, nil
}

func masterKeyHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, ErrInvalidOnBehalfOf) {
		return http.StatusBadRequest
	}
	if domain.ErrorCode(err) == "INTERNAL_ERROR" {
		return http.StatusServiceUnavailable
	}
	return domain.HTTPStatus(err)
}
