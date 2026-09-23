package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const cookieName = "sinthmux_session"

type OAuthConfig struct{ ClientID, ClientSecret, PublicURL, AuthorizeURL, TokenURL, UserURL string }
type OAuthState struct {
	Verifier string
	Expires  time.Time
}
type Server struct {
	Store           *Store
	OAuth           OAuthConfig
	OnDeviceRevoked func(string)
	mu              sync.Mutex
	states          map[string]OAuthState
	attempts        map[string][]time.Time
}

type contextKey int

const sessionKey contextKey = 1

func NewServer(store *Store, oauth OAuthConfig) *Server {
	if oauth.AuthorizeURL == "" {
		oauth.AuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if oauth.TokenURL == "" {
		oauth.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if oauth.UserURL == "" {
		oauth.UserURL = "https://api.github.com/user"
	}
	return &Server{Store: store, OAuth: oauth, states: map[string]OAuthState{}, attempts: map[string][]time.Time{}}
}
func FromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey).(Session)
	return s, ok
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readBody(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		respond(w, 400, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	public, err := url.Parse(s.OAuth.PublicURL)
	if err != nil {
		return false
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == public.Scheme && parsed.Host == public.Host
}

func (s *Server) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			respond(w, 401, map[string]string{"error": "login required"})
			return
		}
		session, err := s.Store.Session(r.Context(), cookie.Value)
		if err != nil {
			respond(w, 401, map[string]string{"error": "login required"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !s.originAllowed(r) || r.Header.Get("X-Sinthmux-CSRF") != session.CSRF {
				respond(w, 403, map[string]string{"error": "CSRF check failed"})
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, session)))
	})
}

func (s *Server) Device(action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := FromContext(r.Context())
		if !ok {
			respond(w, 401, map[string]string{"error": "login required"})
			return
		}
		_, role, err := s.Store.DeviceRole(r.Context(), session.User.ID, chi.URLParam(r, "deviceId"))
		if err != nil {
			respond(w, 404, map[string]string{"error": "device not found"})
			return
		}
		if !Allowed(role, action) {
			respond(w, 403, map[string]string{"error": "permission denied"})
			return
		}
		next(w, r)
	}
}

func (s *Server) Mount(r chi.Router) {
	r.Get("/api/v1/auth/github/start", s.githubStart)
	r.Get("/api/v1/auth/github/callback", s.githubCallback)
	r.Post("/api/v1/auth/token", s.tokenLogin)
	r.Group(func(r chi.Router) {
		r.Use(s.Require)
		r.Get("/api/v1/auth/me", s.me)
		r.Post("/api/v1/auth/logout", s.logout)
		r.Get("/api/v1/auth/tokens", s.tokens)
		r.Post("/api/v1/auth/tokens", s.createToken)
		r.Delete("/api/v1/auth/tokens/{tokenId}", s.revokeToken)
		r.Get("/api/v1/spaces", s.spaces)
		r.Post("/api/v1/spaces", s.createSpace)
		r.Post("/api/v1/spaces/{spaceId}/members", s.addMember)
		r.Get("/api/v1/spaces/{spaceId}/members", s.members)
		r.Patch("/api/v1/spaces/{spaceId}/members/{userId}", s.changeMember)
		r.Delete("/api/v1/spaces/{spaceId}/members/{userId}", s.removeMember)
		r.Post("/api/v1/spaces/{spaceId}/devices", s.createDevice)
		r.Delete("/api/v1/spaces/{spaceId}/devices/{deviceId}", s.revokeDevice)
		r.Get("/api/v1/spaces/{spaceId}/audit", s.auditEvents)
	})
}

func (s *Server) setCookie(w http.ResponseWriter, value string) {
	public, _ := url.Parse(s.OAuth.PublicURL)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, Secure: public != nil && public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	public, _ := url.Parse(s.OAuth.PublicURL)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", HttpOnly: true, Secure: public != nil && public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	spaces, err := s.Store.Spaces(r.Context(), session.User.ID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 200, map[string]any{"user": session.User, "spaces": spaces, "csrf": session.CSRF})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	_ = s.Store.RevokeSession(r.Context(), session.IDHash)
	s.Store.Audit(r.Context(), session.User.ID, "", session.User.ID, "auth.logout", "ok")
	s.clearCookie(w)
	w.WriteHeader(204)
}

func (s *Server) rateLimit(key string) bool {
	if host, _, err := net.SplitHostPort(key); err == nil {
		key = host
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.attempts) > 10000 {
		for address, times := range s.attempts {
			if len(times) == 0 || now.Sub(times[len(times)-1]) > time.Minute {
				delete(s.attempts, address)
			}
		}
		if len(s.attempts) > 10000 && s.attempts[key] == nil {
			return false
		}
	}
	recent := s.attempts[key][:0]
	for _, attempt := range s.attempts[key] {
		if now.Sub(attempt) < time.Minute {
			recent = append(recent, attempt)
		}
	}
	if len(recent) >= 10 {
		s.attempts[key] = recent
		return false
	}
	s.attempts[key] = append(recent, now)
	return true
}

func (s *Server) tokenLogin(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		respond(w, 403, map[string]string{"error": "origin denied"})
		return
	}
	if !s.rateLimit(r.RemoteAddr) {
		respond(w, 429, map[string]string{"error": "too many attempts"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	user, tokenID, err := s.Store.TokenUser(r.Context(), body.Token)
	if err != nil {
		s.Store.Audit(r.Context(), "", "", r.RemoteAddr, "auth.token_login", "denied")
		respond(w, 401, map[string]string{"error": "invalid login token"})
		return
	}
	secret, _, err := s.Store.NewSession(r.Context(), user.ID, tokenID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	s.setCookie(w, secret)
	s.Store.Audit(r.Context(), user.ID, "", user.ID, "auth.token_login", "ok")
	respond(w, 200, map[string]any{"user": user})
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	items, err := s.Store.Tokens(r.Context(), session.User.ID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 200, map[string]any{"tokens": items})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) < 1 || len(body.Name) > 80 {
		respond(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	session, _ := FromContext(r.Context())
	token, err := s.Store.NewToken(r.Context(), session.User.ID, body.Name, 90*24*time.Hour)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, "", session.User.ID, "auth.token_create", "ok")
	respond(w, 201, map[string]string{"token": token})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	err := s.Store.RevokeToken(r.Context(), session.User.ID, chi.URLParam(r, "tokenId"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "token not found"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, "", chi.URLParam(r, "tokenId"), "auth.token_revoke", "ok")
	w.WriteHeader(204)
}

func (s *Server) spaces(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	items, err := s.Store.Spaces(r.Context(), session.User.ID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 200, map[string]any{"spaces": items})
}

func (s *Server) createSpace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) < 1 || len(body.Name) > 80 {
		respond(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	session, _ := FromContext(r.Context())
	space, err := s.Store.CreateSpace(r.Context(), session.User.ID, body.Name)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, space.ID, space.ID, "space.create", "ok")
	respond(w, 201, space)
}

func (s *Server) auditEvents(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "audit") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	items, err := s.Store.AuditEvents(r.Context(), spaceID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 200, map[string]any{"events": items})
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Role     string `json:"role"`
		GithubID int64  `json:"githubId"`
	}
	if !readBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if (body.GithubID == 0 && (len(body.Name) < 1 || len(body.Name) > 80)) || body.Role == "owner" || !Allowed(body.Role, "list") {
		respond(w, 400, map[string]string{"error": "invalid member"})
		return
	}
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "member") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	var user User
	if body.GithubID > 0 {
		user, err = s.Store.AddGithubMember(r.Context(), spaceID, body.GithubID, body.Role)
	} else {
		user, err = s.Store.AddMember(r.Context(), spaceID, body.Name, body.Role)
	}
	if err != nil {
		respond(w, 400, map[string]string{"error": "could not create member"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, spaceID, user.ID, "member.create", "ok")
	if body.GithubID > 0 {
		respond(w, 201, map[string]any{"user": user})
		return
	}
	token, err := s.Store.NewToken(r.Context(), user.ID, "初始登录令牌", 7*24*time.Hour)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 201, map[string]any{"user": user, "token": token})
}

func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	if _, err := s.Store.Role(r.Context(), session.User.ID, spaceID); err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	items, err := s.Store.Members(r.Context(), spaceID)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	respond(w, 200, map[string]any{"members": items})
}

func (s *Server) changeMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if !readBody(w, r, &body) {
		return
	}
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "member") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	userID := chi.URLParam(r, "userId")
	if err := s.Store.ChangeMemberRole(r.Context(), spaceID, userID, body.Role); err != nil {
		respond(w, 400, map[string]string{"error": "role change denied"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, spaceID, userID, "member.role", "ok")
	w.WriteHeader(204)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "member") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	userID := chi.URLParam(r, "userId")
	if err := s.Store.RemoveMember(r.Context(), spaceID, userID); err != nil {
		respond(w, 400, map[string]string{"error": "member removal denied"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, spaceID, userID, "member.remove", "ok")
	w.WriteHeader(204)
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "device") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	deviceID := chi.URLParam(r, "deviceId")
	if err := s.Store.RevokeDevice(r.Context(), spaceID, deviceID); err != nil {
		respond(w, 404, map[string]string{"error": "device not found"})
		return
	}
	if s.OnDeviceRevoked != nil {
		s.OnDeviceRevoked(deviceID)
	}
	s.Store.Audit(r.Context(), session.User.ID, spaceID, deviceID, "device.revoke", "ok")
	w.WriteHeader(204)
}

func (s *Server) createDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if len(body.Name) < 1 || len(body.Name) > 80 {
		respond(w, 400, map[string]string{"error": "invalid name"})
		return
	}
	session, _ := FromContext(r.Context())
	spaceID := chi.URLParam(r, "spaceId")
	role, err := s.Store.Role(r.Context(), session.User.ID, spaceID)
	if err != nil {
		respond(w, 404, map[string]string{"error": "space not found"})
		return
	}
	if !Allowed(role, "device") {
		respond(w, 403, map[string]string{"error": "permission denied"})
		return
	}
	device, token, err := s.Store.NewDevice(r.Context(), spaceID, body.Name)
	if err != nil {
		respond(w, 500, map[string]string{"error": "database error"})
		return
	}
	s.Store.Audit(r.Context(), session.User.ID, spaceID, device.ID, "device.create", "ok")
	respond(w, 201, map[string]any{"device": device, "token": token})
}

func (s *Server) githubStart(w http.ResponseWriter, r *http.Request) {
	if s.OAuth.ClientID == "" || s.OAuth.ClientSecret == "" {
		respond(w, 503, map[string]string{"error": "GitHub login not configured"})
		return
	}
	if !s.rateLimit(r.RemoteAddr) {
		respond(w, 429, map[string]string{"error": "too many attempts"})
		return
	}
	state, err := Random()
	if err != nil {
		respond(w, 500, map[string]string{"error": "randomness unavailable"})
		return
	}
	verifier, err := Random()
	if err != nil {
		respond(w, 500, map[string]string{"error": "randomness unavailable"})
		return
	}
	s.mu.Lock()
	for old, transaction := range s.states {
		if time.Now().After(transaction.Expires) {
			delete(s.states, old)
		}
	}
	if len(s.states) > 10000 {
		s.mu.Unlock()
		respond(w, 429, map[string]string{"error": "too many login attempts"})
		return
	}
	s.states[state] = OAuthState{Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	public, _ := url.Parse(s.OAuth.PublicURL)
	http.SetCookie(w, &http.Cookie{Name: "sinthmux_oauth_state", Value: state, Path: "/api/v1/auth/github", HttpOnly: true, Secure: public != nil && public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 600})
	challenge := sha256.Sum256([]byte(verifier))
	values := url.Values{"client_id": {s.OAuth.ClientID}, "redirect_uri": {strings.TrimRight(s.OAuth.PublicURL, "/") + "/api/v1/auth/github/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, s.OAuth.AuthorizeURL+"?"+values.Encode(), http.StatusFound)
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("sinthmux_oauth_state")
	if err != nil || state == "" || cookie.Value != state {
		respond(w, 400, map[string]string{"error": "invalid OAuth state"})
		return
	}
	s.mu.Lock()
	transaction, ok := s.states[state]
	delete(s.states, state)
	s.mu.Unlock()
	if !ok || time.Now().After(transaction.Expires) {
		respond(w, 400, map[string]string{"error": "expired OAuth state"})
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		respond(w, 400, map[string]string{"error": "missing OAuth code"})
		return
	}
	values := url.Values{"client_id": {s.OAuth.ClientID}, "client_secret": {s.OAuth.ClientSecret}, "code": {code}, "redirect_uri": {strings.TrimRight(s.OAuth.PublicURL, "/") + "/api/v1/auth/github/callback"}, "code_verifier": {transaction.Verifier}}
	req, err := http.NewRequestWithContext(r.Context(), "POST", s.OAuth.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		respond(w, 500, map[string]string{"error": "OAuth request failed"})
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		respond(w, 502, map[string]string{"error": "GitHub unavailable"})
		return
	}
	defer response.Body.Close()
	var exchange struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&exchange) != nil || exchange.AccessToken == "" {
		respond(w, 502, map[string]string{"error": "GitHub authorization failed"})
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), "GET", s.OAuth.UserURL, nil)
	if err != nil {
		respond(w, 500, map[string]string{"error": "GitHub request failed"})
		return
	}
	request.Header.Set("Authorization", "Bearer "+exchange.AccessToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	profile, err := client.Do(request)
	if err != nil {
		respond(w, 502, map[string]string{"error": "GitHub unavailable"})
		return
	}
	defer profile.Body.Close()
	var info struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if profile.StatusCode != 200 || json.NewDecoder(io.LimitReader(profile.Body, 4096)).Decode(&info) != nil || info.ID <= 0 {
		respond(w, 502, map[string]string{"error": "GitHub profile failed"})
		return
	}
	user, err := s.Store.GithubUser(r.Context(), info.ID, info.Login)
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
	s.Store.Audit(r.Context(), user.ID, "", user.ID, "auth.github_login", "ok")
	http.Redirect(w, r, s.OAuth.PublicURL, http.StatusFound)
}
