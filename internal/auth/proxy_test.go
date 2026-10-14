package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test-only value; deployments generate independent random proxy secrets.
const testProxySecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func proxyRequest(ip, secret string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader("{} trailing"))
	r.RemoteAddr = "172.20.0.2:12345"
	if ip != "" {
		r.Header.Set(clientIPHeader, ip)
	}
	if secret != "" {
		r.Header.Set(proxySecretHeader, secret)
	}
	return r
}

func TestProxySecretConfiguration(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("x", 64), strings.Repeat("a", 62), strings.Repeat("a", 66)} {
		if err := NewServer(nil, OAuthConfig{}).SetProxySecret(value); err == nil {
			t.Fatal("invalid proxy secret accepted")
		}
	}
	s := NewServer(nil, OAuthConfig{})
	if err := s.SetProxySecret(testProxySecret); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProxySecret(""); err != nil || s.proxySecret != "" {
		t.Fatal("empty secret did not disable forwarded-IP trust")
	}
}

func TestClientAddressRequiresAuthenticatedSingleIP(t *testing.T) {
	for _, test := range []struct {
		name, ip, secret, want string
		disabled               bool
		duplicateIP            bool
		duplicateSecret        bool
	}{
		{name: "IPv4", ip: "192.0.2.1", secret: testProxySecret, want: "192.0.2.1"},
		{name: "IPv6 canonical", ip: "2001:db8:0:0::1", secret: testProxySecret, want: "2001:db8::1"},
		{name: "mapped IPv4", ip: "::ffff:192.0.2.1", secret: testProxySecret, want: "192.0.2.1"},
		{name: "disabled", ip: "192.0.2.1", secret: testProxySecret, disabled: true},
		{name: "missing credential", ip: "192.0.2.1"},
		{name: "forged credential", ip: "192.0.2.1", secret: strings.Repeat("f", 64)},
		{name: "missing IP", secret: testProxySecret},
		{name: "list rejected", ip: "192.0.2.1, 192.0.2.2", secret: testProxySecret},
		{name: "hostname rejected", ip: "client.example", secret: testProxySecret},
		{name: "port rejected", ip: "192.0.2.1:1234", secret: testProxySecret},
		{name: "zone rejected", ip: "fe80::1%en0", secret: testProxySecret},
		{name: "duplicate IP", ip: "192.0.2.1", secret: testProxySecret, duplicateIP: true},
		{name: "duplicate credential", ip: "192.0.2.1", secret: testProxySecret, duplicateSecret: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := NewServer(nil, OAuthConfig{})
			if !test.disabled {
				if err := s.SetProxySecret(testProxySecret); err != nil {
					t.Fatal(err)
				}
			}
			r := proxyRequest(test.ip, test.secret)
			r.Header.Set("X-Forwarded-For", "198.51.100.20")
			r.Header.Set("X-Real-IP", "198.51.100.21")
			r.Header.Set("Forwarded", "for=198.51.100.22")
			if test.duplicateIP {
				r.Header.Add(clientIPHeader, test.ip)
			}
			if test.duplicateSecret {
				r.Header.Add(proxySecretHeader, test.secret)
			}
			want := test.want
			if want == "" {
				want = r.RemoteAddr
			}
			if got := s.clientAddress(r); got != want {
				t.Fatalf("got %q; want %q", got, want)
			}
		})
	}
}

func TestTokenLoginRateLimitSeparatesAuthenticatedProxyClients(t *testing.T) {
	s := NewServer(nil, OAuthConfig{})
	if err := s.SetProxySecret(testProxySecret); err != nil {
		t.Fatal(err)
	}
	request := func(ip, credential string, want int) {
		t.Helper()
		r := proxyRequest(ip, credential)
		w := httptest.NewRecorder()
		// Invalid JSON stops before any database access, but after the limiter.
		s.tokenLogin(w, r)
		if w.Code != want {
			t.Fatalf("status=%d, want=%d", w.Code, want)
		}
	}
	for range 10 {
		request("192.0.2.1", testProxySecret, http.StatusBadRequest)
	}
	request("192.0.2.1", testProxySecret, http.StatusTooManyRequests)
	request("::ffff:192.0.2.1", testProxySecret, http.StatusTooManyRequests)
	request("192.0.2.2", testProxySecret, http.StatusBadRequest)
	// Forged assertions must consume the actual socket peer's shared bucket.
	for range 10 {
		request("192.0.2.3", "forged", http.StatusBadRequest)
	}
	request("192.0.2.4", "forged", http.StatusTooManyRequests)
	request("192.0.2.5", "", http.StatusTooManyRequests)
	// An unauthenticated caller cannot exhaust a real client's bucket.
	request("192.0.2.3", testProxySecret, http.StatusBadRequest)
}
