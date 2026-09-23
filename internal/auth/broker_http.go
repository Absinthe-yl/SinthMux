package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Server) brokerStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.rateLimit(r.RemoteAddr) {
		respond(w, 429, map[string]string{"error": "too many attempts"})
		return
	}
	state, err := Random()
	if err != nil {
		respond(w, 500, map[string]string{"error": "randomness unavailable"})
		return
	}
	s.mu.Lock()
	for id, item := range s.states {
		if time.Now().After(item.Expires) {
			delete(s.states, id)
		}
	}
	if len(s.states) >= 10000 {
		s.mu.Unlock()
		respond(w, 429, map[string]string{"error": "too many login attempts"})
		return
	}
	s.states[state] = OAuthState{Expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	public, _ := url.Parse(s.OAuth.PublicURL)
	http.SetCookie(w, &http.Cookie{Name: "sinthmux_oauth_state", Value: state, Path: "/api/v1/auth/github", HttpOnly: true, Secure: public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 600})
	callback := strings.TrimRight(s.OAuth.PublicURL, "/") + "/api/v1/auth/github/broker/callback"
	destination, _ := url.Parse(strings.TrimRight(s.OAuth.BrokerURL, "/") + "/start")
	destination.RawQuery = url.Values{"return_to": {callback}, "state": {state}}.Encode()
	http.Redirect(w, r, destination.String(), http.StatusFound)
}

func (s *Server) brokerCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.BrokerEnabled() {
		respond(w, 503, map[string]string{"error": "login broker not configured"})
		return
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("sinthmux_oauth_state")
	if err != nil || state == "" || cookie.Value != state {
		respond(w, 400, map[string]string{"error": "invalid login state"})
		return
	}
	s.mu.Lock()
	item, ok := s.states[state]
	delete(s.states, state)
	s.mu.Unlock()
	if !ok || time.Now().After(item.Expires) {
		respond(w, 400, map[string]string{"error": "expired login state"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sinthmux_oauth_state", Path: "/api/v1/auth/github", MaxAge: -1, HttpOnly: true})
	code := r.URL.Query().Get("code")
	if len(code) < 32 || len(code) > 128 {
		respond(w, 400, map[string]string{"error": "invalid exchange code"})
		return
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(s.OAuth.BrokerURL, "/")+"/exchange", bytes.NewReader(body))
	if err != nil {
		respond(w, 500, map[string]string{"error": "login exchange failed"})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		respond(w, 502, map[string]string{"error": "login broker unavailable"})
		return
	}
	defer response.Body.Close()
	var exchanged struct {
		Ticket string `json:"ticket"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&exchanged) != nil {
		respond(w, 502, map[string]string{"error": "login exchange failed"})
		return
	}
	key, _ := base64.RawURLEncoding.DecodeString(s.OAuth.BrokerPublicKey)
	callback := strings.TrimRight(s.OAuth.PublicURL, "/") + "/api/v1/auth/github/broker/callback"
	ticket, err := VerifyBrokerTicket(ed25519.PublicKey(key), exchanged.Ticket, callback, state)
	if err != nil {
		respond(w, 401, map[string]string{"error": "invalid login ticket"})
		return
	}
	user, err := s.Store.GithubUser(r.Context(), ticket.GithubID, ticket.Login)
	if err != nil {
		respond(w, 403, map[string]string{"error": "user disabled"})
		return
	}
	secret, _, err := s.Store.NewSession(r.Context(), user.ID, "")
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	s.setCookie(w, secret)
	s.Store.Audit(r.Context(), user.ID, "", user.ID, "auth.github_broker_login", "ok")
	http.Redirect(w, r, s.OAuth.PublicURL, http.StatusFound)
}
