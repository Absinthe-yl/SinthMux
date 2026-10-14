package auth

import (
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/netip"
)

const (
	proxySecretHeader = "X-Sinthmux-Proxy-Secret"
	clientIPHeader    = "X-Sinthmux-Client-IP"
)

// SetProxySecret enables the authenticated client-IP assertion made by our
// reverse proxy. Empty disables it. Never trust a public forwarding header by
// itself, or a client could rotate its claimed IP to evade login throttling.
func (s *Server) SetProxySecret(secret string) error {
	if secret == "" {
		s.proxySecret = ""
		return nil
	}
	decoded, err := hex.DecodeString(secret)
	if err != nil || len(decoded) != 32 {
		return ErrDenied
	}
	s.proxySecret = secret
	return nil
}

// clientAddress is used only for rate limiting, not identity or authorization.
// Caddy overwrites both private headers using its actual socket peer address.
// Direct access, a disabled/misconfigured proxy, malformed assertions and all
// untrusted Forwarded/X-Forwarded-For/X-Real-IP values fall back to RemoteAddr.
func (s *Server) clientAddress(r *http.Request) string {
	if s.proxySecret == "" || len(r.Header.Values(proxySecretHeader)) != 1 || len(r.Header.Values(clientIPHeader)) != 1 ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get(proxySecretHeader)), []byte(s.proxySecret)) != 1 {
		return r.RemoteAddr
	}
	ip, err := netip.ParseAddr(r.Header.Get(clientIPHeader))
	if err != nil || ip.Zone() != "" {
		return r.RemoteAddr
	}
	return ip.Unmap().String()
}
