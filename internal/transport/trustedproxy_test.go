package transport

import (
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
)

func proxyAPI(cidrs ...string) *API {
	return &API{Service: &application.Service{Config: config.Config{TrustedProxyCIDRs: cidrs}}}
}

func TestSourceIPIgnoresForwardingHeadersFromUntrustedPeer(t *testing.T) {
	api := proxyAPI("10.0.0.0/8")
	headers := map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Real-IP": "198.51.100.8"}
	get := func(k string) string { return headers[k] }
	if got := api.sourceIP("203.0.113.9:1234", get); got != "203.0.113.9" {
		t.Fatalf("untrusted peer resolved to %q", got)
	}
}

func TestSourceIPUsesRightmostUntrustedHop(t *testing.T) {
	api := proxyAPI("10.0.0.0/8")
	headers := map[string]string{"X-Forwarded-For": "198.51.100.7, 10.2.0.4, 10.1.2.3"}
	get := func(k string) string { return headers[k] }
	if got := api.sourceIP("10.1.2.3:1234", get); got != "198.51.100.7" {
		t.Fatalf("trusted peer resolved to %q", got)
	}
}

func TestSourceIPFallsBackToPeerOrRealIP(t *testing.T) {
	api := proxyAPI("10.0.0.0/8")
	headers := map[string]string{"X-Real-IP": "198.51.100.8"}
	get := func(k string) string { return headers[k] }
	if got := api.sourceIP("10.1.2.3:1234", get); got != "198.51.100.8" {
		t.Fatalf("real ip fallback: %q", got)
	}
	headers = nil
	if got := api.sourceIP("10.1.2.3:1234", get); got != "10.1.2.3" {
		t.Fatalf("peer fallback: %q", got)
	}
}

func TestSourceIPHandlesIPv6AndMalformedValues(t *testing.T) {
	api := proxyAPI("2001:db8::1/128")
	headers := map[string]string{"X-Forwarded-For": "not-an-ip, 2001:db8:1::5, 2001:db8::1"}
	get := func(k string) string { return headers[k] }
	if got := api.sourceIP("[2001:db8::1]:443", get); got != "2001:db8:1::5" {
		t.Fatalf("ipv6: %q", got)
	}
	if got := api.sourceIP("[192.0.2.10]:443", get); got != "192.0.2.10" {
		t.Fatalf("ipv4 peer: %q", got)
	}
}
