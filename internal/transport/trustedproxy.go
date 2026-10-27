package transport

import (
	"net"
	"strings"
)

// sourceIP returns the address used for the master-key CIDR allowlist. It only
// trusts forwarding headers when the immediate peer is a configured proxy, and
// walks X-Forwarded-For from right to left so a client-supplied left-hand value
// cannot override the address appended by the nearest trusted proxy.
func (a *API) sourceIP(remoteAddr string, header func(string) string) string {
	peer := remoteIP(remoteAddr)
	if a == nil || a.Service == nil || !inCIDRs(peer, a.Service.Config.TrustedProxyCIDRs) {
		return peer
	}
	if header != nil {
		if xff := header("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if net.ParseIP(candidate) == nil {
					continue
				}
				if !inCIDRs(candidate, a.Service.Config.TrustedProxyCIDRs) {
					return candidate
				}
			}
		}
		if real := strings.TrimSpace(header("X-Real-IP")); net.ParseIP(real) != nil {
			return real
		}
	}
	return peer
}

func inCIDRs(ip string, cidrs []string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, raw := range cidrs {
		_, network, e := net.ParseCIDR(strings.TrimSpace(raw))
		if e == nil && network.Contains(parsed) {
			return true
		}
	}
	return false
}
