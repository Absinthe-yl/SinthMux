package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestRoleMatrix(t *testing.T) {
	for _, tc := range []struct {
		role, action string
		allowed      bool
	}{
		{"owner", "member", true}, {"admin", "member", false},
		{"operator", "input", true}, {"operator", "close", false},
		{"viewer", "list", true}, {"viewer", "input", false},
		{"viewer", "create", false}, {"unknown", "list", false},
	} {
		if got := Allowed(tc.role, tc.action); got != tc.allowed {
			t.Errorf("Allowed(%q,%q)=%v; want %v", tc.role, tc.action, got, tc.allowed)
		}
	}
}

func TestTokenLoginRouteRemainsAvailable(t *testing.T) {
	router := chi.NewRouter()
	NewServer(nil, OAuthConfig{PublicURL: "http://127.0.0.1:5173"}).Mount(router)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader("invalid JSON"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("token login route returned %d; want 400", response.Code)
	}
}

func TestHubStartsBrokerLoginAndRejectsForgedCallback(t *testing.T) {
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	server := NewServer(nil, OAuthConfig{PublicURL: "http://127.0.0.1:5173", BrokerURL: "http://127.0.0.1:8091", BrokerPublicKey: base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey))})
	if !server.GithubEnabled() {
		t.Fatal("broker login unavailable")
	}
	router := chi.NewRouter()
	server.Mount(router)
	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/start", nil))
	if start.Code != http.StatusFound {
		t.Fatalf("start=%d", start.Code)
	}
	destination, err := url.Parse(start.Header().Get("Location"))
	if err != nil || destination.Host != "127.0.0.1:8091" || destination.Path != "/start" {
		t.Fatalf("broker redirect=%q", start.Header().Get("Location"))
	}
	state := destination.Query().Get("state")
	callback := httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/broker/callback?state="+state+"&code=forged", nil)
	callback.AddCookie(start.Result().Cookies()[0])
	response := httptest.NewRecorder()
	router.ServeHTTP(response, callback)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("forged callback=%d", response.Code)
	}
}

func TestFormalStoreIntegration(t *testing.T) {
	dsn := os.Getenv("SINTHMUX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SINTHMUX_TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	githubID := time.Now().UnixNano()
	owner, err := s.GithubUser(ctx, githubID, "test-owner")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.GithubUser(ctx, githubID, "renamed-owner")
	if err != nil || again.ID != owner.ID {
		t.Fatalf("GitHub identity changed: %+v %v", again, err)
	}
	spaces, err := s.Spaces(ctx, owner.ID)
	if err != nil || len(spaces) != 1 || spaces[0].Role != "owner" {
		t.Fatalf("personal space: %+v %v", spaces, err)
	}
	spaceID := spaces[0].ID
	token, err := s.NewToken(ctx, owner.ID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tokenUser, tokenID, err := s.TokenUser(ctx, token)
	if err != nil || tokenUser.ID != owner.ID {
		t.Fatalf("token login: %+v %v", tokenUser, err)
	}
	sessionSecret, _, err := s.NewSession(ctx, owner.ID, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Session(ctx, sessionSecret)
	if err != nil || session.User.ID != owner.ID {
		t.Fatalf("session: %+v %v", session, err)
	}
	device, deviceToken, err := s.NewDevice(ctx, spaceID, "test-device")
	if err != nil {
		t.Fatal(err)
	}
	if !s.AuthenticateDevice(ctx, device.ID, deviceToken) || s.AuthenticateDevice(ctx, device.ID, "smd_"+device.ID+"_wrong") {
		t.Fatal("device credential validation failed")
	}
	pairing, expires, err := s.NewDevicePairing(ctx, spaceID, owner.ID, "paired-device")
	if err != nil || time.Until(expires) <= 0 {
		t.Fatalf("new pairing: %v", err)
	}
	paired, pairedToken, err := s.RedeemDevicePairing(ctx, pairing)
	if err != nil || paired.Name != "paired-device" || !s.AuthenticateDevice(ctx, paired.ID, pairedToken) {
		t.Fatalf("redeem pairing: %+v %v", paired, err)
	}
	if _, _, err := s.RedeemDevicePairing(ctx, pairing); err == nil {
		t.Fatal("pairing code was accepted twice")
	}
	expired, _, err := s.NewDevicePairing(ctx, spaceID, owner.ID, "expired-device")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE device_pairings SET expires_at=now()-interval '1 second' WHERE code_hash=$1`, digest(expired)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RedeemDevicePairing(ctx, expired); err == nil {
		t.Fatal("expired pairing code was accepted")
	}
	manager := relay.NewManager()
	registry := devices.NewRegistry()
	connectorHTTP := relay.ConnectorHandler{Registry: registry, Manager: manager, AuthenticateDevice: func(ctx context.Context, id, authorization string) bool {
		return strings.HasPrefix(authorization, "Bearer ") && s.AuthenticateDevice(ctx, id, strings.TrimPrefix(authorization, "Bearer "))
	}}
	connectorServer := httptest.NewServer(connectorHTTP)
	defer connectorServer.Close()
	wsURL := "ws" + strings.TrimPrefix(connectorServer.URL, "http")
	_, _, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer bad"}, "X-Sinthmux-Device-ID": {device.ID}}})
	if err == nil {
		t.Fatal("connector accepted invalid credential")
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + deviceToken}, "X-Sinthmux-Device-ID": {device.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	wrongHello, _ := json.Marshal(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: "other-device"}})
	if err := conn.Write(ctx, websocket.MessageText, wrongHello); err != nil {
		t.Fatal(err)
	}
	_, _, _ = conn.Read(ctx)
	conn.CloseNow()
	if manager.Online("other-device") {
		t.Fatal("connector changed its authenticated device ID")
	}
	conn, _, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + deviceToken}, "X-Sinthmux-Device-ID": {device.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	validHello, _ := json.Marshal(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: device.ID, Name: "connector"}})
	if err := conn.Write(ctx, websocket.MessageText, validHello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if !manager.Online(device.ID) {
		t.Fatal("authenticated connector not registered")
	}
	conn.CloseNow()
	if !s.TerminalAllowed(ctx, owner.ID, device.ID, spaceID, session.IDHash) {
		t.Fatal("owner cannot use own terminal")
	}
	other, err := s.GithubUser(ctx, githubID+1, "test-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeviceRole(ctx, other.ID, device.ID); err == nil {
		t.Fatal("cross-space device visible")
	}
	viewer, err := s.AddMember(ctx, spaceID, "viewer", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, role, err := s.DeviceRole(ctx, viewer.ID, device.ID); err != nil || role != "viewer" {
		t.Fatalf("viewer role: %s %v", role, err)
	}
	viewerSecret, viewerCSRF, err := s.NewSession(ctx, viewer.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	viewerSession, err := s.Session(ctx, viewerSecret)
	if err != nil {
		t.Fatal(err)
	}
	if s.TerminalAllowed(ctx, viewer.ID, device.ID, spaceID, viewerSession.IDHash) {
		t.Fatal("viewer received terminal input permission")
	}
	authServer := NewServer(s, OAuthConfig{PublicURL: "http://127.0.0.1:5173"})
	pairRouter := chi.NewRouter()
	authServer.Mount(pairRouter)
	issue := func(secret, csrf, name string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/spaces/"+spaceID+"/device-pairings", strings.NewReader(`{"name":"`+name+`"}`))
		req.AddCookie(&http.Cookie{Name: cookieName, Value: secret})
		req.Header.Set("X-Sinthmux-CSRF", csrf)
		response := httptest.NewRecorder()
		pairRouter.ServeHTTP(response, req)
		return response
	}
	if got := issue(viewerSecret, viewerCSRF, "denied").Code; got != 403 {
		t.Fatalf("viewer pairing status=%d", got)
	}
	ownerCSRF := session.CSRF
	issued := issue(sessionSecret, ownerCSRF, "route-paired")
	if issued.Code != 201 {
		t.Fatalf("owner pairing status=%d body=%s", issued.Code, issued.Body.String())
	}
	var issueBody struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &issueBody); err != nil {
		t.Fatal(err)
	}
	redeemRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/connectors/pair", strings.NewReader(`{"code":"`+issueBody.Code+`"}`))
		response := httptest.NewRecorder()
		pairRouter.ServeHTTP(response, req)
		return response
	}
	if got := redeemRequest().Code; got != 201 {
		t.Fatalf("pair route status=%d", got)
	}
	if got := redeemRequest().Code; got != 401 {
		t.Fatalf("pair replay status=%d", got)
	}
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(authServer.Require)
		r.Get("/devices/{deviceId}", authServer.Device("list", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		r.Post("/devices/{deviceId}", authServer.Device("input", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	})
	call := func(method, deviceID, secret, csrf string) int {
		req := httptest.NewRequest(method, "http://127.0.0.1:5173/devices/"+deviceID, nil)
		if secret != "" {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: secret})
		}
		if csrf != "" {
			req.Header.Set("X-Sinthmux-CSRF", csrf)
		}
		record := httptest.NewRecorder()
		router.ServeHTTP(record, req)
		return record.Code
	}
	if got := call("GET", device.ID, "", ""); got != 401 {
		t.Fatalf("anonymous status=%d", got)
	}
	if got := call("GET", device.ID, viewerSecret, ""); got != 204 {
		t.Fatalf("viewer listing status=%d", got)
	}
	if got := call("POST", device.ID, viewerSecret, viewerCSRF); got != 403 {
		t.Fatalf("viewer input status=%d", got)
	}
	if got := call("POST", device.ID, viewerSecret, ""); got != 403 {
		t.Fatalf("missing CSRF status=%d", got)
	}
	if got := call("GET", device.ID, sessionSecret, ""); got != 204 {
		t.Fatalf("owner device status=%d", got)
	}
	otherSecret, _, err := s.NewSession(ctx, other.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := call("GET", device.ID, otherSecret, ""); got != 404 {
		t.Fatalf("cross-space status=%d", got)
	}
	if err := s.ChangeMemberRole(ctx, spaceID, viewer.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	if !s.TerminalAllowed(ctx, viewer.ID, device.ID, spaceID, viewerSession.IDHash) {
		t.Fatal("operator denied terminal")
	}
	if err := s.RemoveMember(ctx, spaceID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if s.TerminalAllowed(ctx, viewer.ID, device.ID, spaceID, viewerSession.IDHash) {
		t.Fatal("removed member retained terminal")
	}
	if err := s.RevokeToken(ctx, owner.ID, tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sessionSecret); err == nil {
		t.Fatal("revoked token retained browser session")
	}
	if _, _, err := s.TokenUser(ctx, token); err == nil {
		t.Fatal("revoked token still logs in")
	}
	if err := s.RevokeDevice(ctx, spaceID, device.ID); err != nil {
		t.Fatal(err)
	}
	if s.AuthenticateDevice(ctx, device.ID, deviceToken) {
		t.Fatal("revoked device still authenticates")
	}
	fakeGithub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "test-code" || r.FormValue("code_verifier") == "" {
				t.Error("OAuth code or PKCE verifier missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-access"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer test-access" {
				t.Error("GitHub access token missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": githubID + 2, "login": "oauth-user"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGithub.Close()
	oauth := NewServer(s, OAuthConfig{ClientID: "test-client", ClientSecret: "test-secret", PublicURL: "http://127.0.0.1:5173", AuthorizeURL: fakeGithub.URL + "/authorize", TokenURL: fakeGithub.URL + "/token", UserURL: fakeGithub.URL + "/user"})
	start := httptest.NewRecorder()
	oauth.githubStart(start, httptest.NewRequest("GET", "http://127.0.0.1:5173/api/v1/auth/github/start", nil))
	if start.Code != 302 {
		t.Fatalf("OAuth start=%d: %s", start.Code, start.Body.String())
	}
	redirect, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := redirect.Query().Get("state")
	verifier := oauth.states[state].Verifier
	challenge := sha256.Sum256([]byte(verifier))
	if state == "" || redirect.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) {
		t.Fatal("OAuth state or PKCE challenge missing")
	}
	var stateCookie *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == "sinthmux_oauth_state" {
			stateCookie = cookie
		}
	}
	if stateCookie == nil {
		t.Fatal("OAuth browser binding missing")
	}
	callbackURL := "http://127.0.0.1:5173/api/v1/auth/github/callback?code=test-code&state=" + state
	wrong := httptest.NewRecorder()
	oauth.githubCallback(wrong, httptest.NewRequest("GET", callbackURL, nil))
	if wrong.Code != 400 {
		t.Fatalf("unbound OAuth callback=%d", wrong.Code)
	}
	callbackRequest := httptest.NewRequest("GET", callbackURL, nil)
	callbackRequest.AddCookie(stateCookie)
	callback := httptest.NewRecorder()
	oauth.githubCallback(callback, callbackRequest)
	if callback.Code != 302 {
		t.Fatalf("OAuth callback=%d: %s", callback.Code, callback.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == cookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("OAuth browser session missing")
	}
	loggedIn, err := s.Session(ctx, sessionCookie.Value)
	if err != nil || loggedIn.User.GithubID != githubID+2 || !strings.EqualFold(loggedIn.User.Name, "oauth-user") {
		t.Fatalf("OAuth user mapping: %+v %v", loggedIn.User, err)
	}
	replayed := httptest.NewRecorder()
	oauth.githubCallback(replayed, callbackRequest)
	if replayed.Code != 400 {
		t.Fatalf("OAuth state replay=%d", replayed.Code)
	}
}
