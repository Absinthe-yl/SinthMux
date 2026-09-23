package loginbroker

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sinthmux/sinthmux/internal/auth"
)

func TestBrokerExchangesOneTimeCodeForBoundIdentity(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code_verifier") == "" || r.FormValue("client_secret") != "test-secret" {
				t.Error("missing OAuth verifier or secret")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-access"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer test-access" {
				t.Error("missing GitHub access token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": "alice"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	broker, err := New(Config{PublicURL: "http://127.0.0.1:8091", ClientID: "test-id", ClientSecret: "test-secret", SigningKey: key, AuthorizeURL: github.URL + "/authorize", TokenURL: github.URL + "/token", UserURL: github.URL + "/user"})
	if err != nil {
		t.Fatal(err)
	}
	returnTo := "http://127.0.0.1:5173/api/v1/auth/github/broker/callback"
	start := httptest.NewRecorder()
	broker.Handler().ServeHTTP(start, httptest.NewRequest("GET", "/start?"+url.Values{"return_to": {returnTo}, "state": {strings.Repeat("a", 32)}}.Encode(), nil))
	if start.Code != 302 {
		t.Fatalf("start=%d: %s", start.Code, start.Body.String())
	}
	authorize, _ := url.Parse(start.Header().Get("Location"))
	if authorize.Query().Get("code_challenge") == "" {
		t.Fatal("missing PKCE challenge")
	}
	callback := httptest.NewRequest("GET", "/callback?code=ok&state="+authorize.Query().Get("state"), nil)
	callback.AddCookie(start.Result().Cookies()[0])
	completed := httptest.NewRecorder()
	broker.Handler().ServeHTTP(completed, callback)
	if completed.Code != 302 {
		t.Fatalf("callback=%d: %s", completed.Code, completed.Body.String())
	}
	redirect, _ := url.Parse(completed.Header().Get("Location"))
	if redirect.Scheme+"://"+redirect.Host+redirect.Path != returnTo || redirect.Query().Get("ticket") != "" {
		t.Fatal("identity leaked in redirect")
	}
	code := redirect.Query().Get("code")
	exchange := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		broker.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/exchange", strings.NewReader(`{"code":"`+code+`"}`)))
		return response
	}
	issued := exchange()
	if issued.Code != 200 {
		t.Fatalf("exchange=%d: %s", issued.Code, issued.Body.String())
	}
	var data struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	ticket, err := auth.VerifyBrokerTicket(key.Public().(ed25519.PublicKey), data.Ticket, returnTo, strings.Repeat("a", 32))
	if err != nil || ticket.GithubID != 42 || ticket.Login != "alice" {
		t.Fatalf("ticket=%+v err=%v", ticket, err)
	}
	if exchange().Code == 200 {
		t.Fatal("exchange code replayed")
	}
	if _, err := auth.VerifyBrokerTicket(key.Public().(ed25519.PublicKey), data.Ticket, returnTo, "different-state"); err == nil {
		t.Fatal("ticket accepted under another state")
	}
	if _, err := auth.VerifyBrokerTicket(key.Public().(ed25519.PublicKey), data.Ticket, "https://other.example/api/v1/auth/github/broker/callback", strings.Repeat("a", 32)); err == nil {
		t.Fatal("ticket accepted by another Hub")
	}
	if _, err := auth.VerifyBrokerTicket(key.Public().(ed25519.PublicKey), data.Ticket+"x", returnTo, strings.Repeat("a", 32)); err == nil {
		t.Fatal("tampered ticket accepted")
	}
}

func TestBrokerRejectsUntrustedReturnURL(t *testing.T) {
	for _, raw := range []string{"http://evil.example/api/v1/auth/github/broker/callback", "https://evil.example/other", "https://evil.example/api/v1/auth/github/broker/callback?next=1"} {
		if validReturnTo(raw) {
			t.Errorf("accepted %q", raw)
		}
	}
}
