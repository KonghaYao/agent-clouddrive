package ocis

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// SpaceProvisionerConfig uses a dedicated high-privilege client to create
// project drives. It is never used for file reads or writes.
type SpaceProvisionerConfig struct {
	URL, Instance                              string
	TokenURL, ClientID, ClientSecret, Audience string
	Scopes                                     []string
	Verified                                   bool
	Client                                     *http.Client
}

type SpaceProvisioner struct {
	cfg    SpaceProvisionerConfig
	client *http.Client
}

func NewSpaceProvisioner(cfg SpaceProvisionerConfig) (*SpaceProvisioner, error) {
	u, e := url.Parse(cfg.URL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ports.ErrInvalid
	}
	tu, e := url.Parse(cfg.TokenURL)
	if e != nil || (tu.Scheme != "http" && tu.Scheme != "https") || tu.Host == "" || tu.User != nil || tu.RawQuery != "" || tu.Fragment != "" {
		return nil, ports.ErrInvalid
	}
	if cfg.Instance == "" || strings.ContainsAny(cfg.Instance, " \t\r\n\x00") || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Audience == "" {
		return nil, ports.ErrInvalid
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid"}
	}
	for _, scope := range cfg.Scopes {
		if scope != "openid" {
			return nil, ports.ErrInvalid
		}
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	if cfg.Client != nil {
		*cl = *cfg.Client
		if cl.Timeout == 0 {
			cl.Timeout = 30 * time.Second
		}
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &SpaceProvisioner{cfg: cfg, client: cl}, nil
}

func (p *SpaceProvisioner) Verified() bool { return p.cfg.Verified }

// ProvisionSpace creates a project drive with a deterministic caller-supplied
// name. It returns ErrConflict on a duplicate so the caller can re-query the
// durable space mapping instead of retrying indefinitely.
func (p *SpaceProvisioner) ProvisionSpace(ctx context.Context, req ports.ProvisionSpaceRequest) (ports.ProvisionedSpace, error) {
	if !p.cfg.Verified {
		return ports.ProvisionedSpace{}, ports.ErrUnverified
	}
	if req.OwnerIssuer == "" || req.OwnerSubject == "" || req.Name == "" || len(req.Name) > 128 || strings.ContainsAny(req.Name, "\x00\r\n/\\") {
		return ports.ProvisionedSpace{}, ports.ErrInvalid
	}
	token, e := p.token(ctx)
	if e != nil {
		return ports.ProvisionedSpace{}, e
	}
	body, e := json.Marshal(map[string]string{"driveType": "project", "name": req.Name})
	if e != nil {
		return ports.ProvisionedSpace{}, ports.ErrInvalid
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.URL, "/")+"/graph/v1.0/drives", bytes.NewReader(body))
	if e != nil {
		return ports.ProvisionedSpace{}, ports.ErrInvalid
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	resp, e := p.client.Do(request)
	if e != nil {
		return ports.ProvisionedSpace{}, &ports.DependencyError{Dependency: "ocis", Operation: "provision_space", Code: "transport_failure", Cause: ports.ErrOutcomeUnknown}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return ports.ProvisionedSpace{}, &ports.DependencyError{Dependency: "ocis", Operation: "provision_space", Status: resp.StatusCode, Code: "space_conflict", Cause: ports.ErrConflict}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cause = ports.ErrUnauthorized
		} else if resp.StatusCode >= 500 {
			cause = ports.ErrOutcomeUnknown
		}
		// Never include the response body or token in the error.
		return ports.ProvisionedSpace{}, &ports.DependencyError{Dependency: "ocis", Operation: "provision_space", Status: resp.StatusCode, Code: "space_rejected", Cause: cause}
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return ports.ProvisionedSpace{}, ports.ErrIntegrity
	}
	var out struct {
		ID   string `json:"id"`
		Root struct {
			ID string `json:"id"`
		} `json:"root"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ID == "" || out.Root.ID == "" || !safeSegment(out.ID) || !safeSegment(out.Root.ID) {
		return ports.ProvisionedSpace{}, &ports.DependencyError{Dependency: "ocis", Operation: "provision_space", Code: "invalid_space", Cause: ports.ErrIntegrity}
	}
	return ports.ProvisionedSpace{NativeInstance: p.cfg.Instance, NativeSpaceID: out.ID, NativeRootResourceID: out.Root.ID}, nil
}

func (p *SpaceProvisioner) token(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
		"audience":      {p.cfg.Audience},
		"scope":         {strings.Join(p.cfg.Scopes, " ")},
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.TokenURL, strings.NewReader(form.Encode()))
	if e != nil {
		return "", ports.ErrInvalid
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := p.client.Do(request)
	if e != nil {
		return "", &ports.DependencyError{Dependency: "ocis", Operation: "provisioner_token", Code: "transport_failure", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cause = ports.ErrUnauthorized
		}
		return "", &ports.DependencyError{Dependency: "ocis", Operation: "provisioner_token", Status: resp.StatusCode, Code: "token_rejected", Cause: cause}
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 {
		return "", ports.ErrIntegrity
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if json.Unmarshal(raw, &out) != nil || out.AccessToken == "" || (out.TokenType != "" && !strings.EqualFold(out.TokenType, "Bearer")) {
		return "", ports.ErrIntegrity
	}
	return out.AccessToken, nil
}

var _ ports.SpaceProvisioner = (*SpaceProvisioner)(nil)
