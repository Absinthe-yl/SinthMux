package loginbroker

import (
	"crypto/ed25519"
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

	"github.com/sinthmux/sinthmux/internal/auth"
)

type Config struct {
	PublicURL, ClientID, ClientSecret string
	AuthorizeURL, TokenURL, UserURL   string
	SigningKey                        ed25519.PrivateKey
}

type pending struct {
	ReturnTo, HubState, Verifier string
	Expires                      time.Time
}
type grant struct {
	Ticket  string
	Expires time.Time
}

type Server struct {
	config   Config
	client   *http.Client
	mu       sync.Mutex
	states   map[string]pending
	grants   map[string]grant
	attempts map[string][]time.Time
}

func New(config Config) (*Server, error) {
	public, err := url.Parse(config.PublicURL)
	if err != nil || !validOrigin(public) || config.ClientID == "" || config.ClientSecret == "" || len(config.SigningKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid login broker configuration")
	}
	if config.AuthorizeURL == "" {
		config.AuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if config.TokenURL == "" {
		config.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if config.UserURL == "" {
		config.UserURL = "https://api.github.com/user"
	}
	return &Server{config: config, client: &http.Client{Timeout: 10 * time.Second}, states: map[string]pending{}, grants: map[string]grant{}, attempts: map[string][]time.Time{}}, nil
}

func validOrigin(u *url.URL) bool {
	return u != nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && (u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")))
}

func validReturnTo(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/v1/auth/github/broker/callback" {
		return false
	}
	return u.Scheme == "https" && u.Host != "" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /start", s.start)
	mux.HandleFunc("GET /callback", s.callback)
	mux.HandleFunc("POST /exchange", s.exchange)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if !s.allowAttempt(r.RemoteAddr) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	returnTo, hubState := r.URL.Query().Get("return_to"), r.URL.Query().Get("state")
	if !validReturnTo(returnTo) || len(hubState) < 16 || len(hubState) > 128 {
		http.Error(w, "invalid login destination", http.StatusBadRequest)
		return
	}
	state, err := auth.Random()
	if err != nil {
		http.Error(w, "randomness unavailable", 500)
		return
	}
	verifier, err := auth.Random()
	if err != nil {
		http.Error(w, "randomness unavailable", 500)
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
		http.Error(w, "too many login attempts", 429)
		return
	}
	s.states[state] = pending{ReturnTo: returnTo, HubState: hubState, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	public, _ := url.Parse(s.config.PublicURL)
	http.SetCookie(w, &http.Cookie{Name: "sinthmux_broker_state", Value: state, Path: "/callback", HttpOnly: true, Secure: public.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 600})
	challenge := sha256.Sum256([]byte(verifier))
	values := url.Values{"client_id": {s.config.ClientID}, "redirect_uri": {s.config.PublicURL + "/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, s.config.AuthorizeURL+"?"+values.Encode(), http.StatusFound)
}

func (s *Server) allowAttempt(remote string) bool {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, values := range s.attempts {
		recent := values[:0]
		for _, at := range values {
			if now.Sub(at) < 5*time.Minute {
				recent = append(recent, at)
			}
		}
		if len(recent) == 0 {
			delete(s.attempts, key)
		} else {
			s.attempts[key] = recent
		}
	}
	if len(s.attempts[ip]) >= 10 {
		return false
	}
	s.attempts[ip] = append(s.attempts[ip], now)
	return true
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	cookie, err := r.Cookie("sinthmux_broker_state")
	if err != nil || state == "" || code == "" || cookie.Value != state {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	item, ok := s.states[state]
	delete(s.states, state)
	s.mu.Unlock()
	if !ok || time.Now().After(item.Expires) {
		http.Error(w, "expired login state", 400)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sinthmux_broker_state", Path: "/callback", MaxAge: -1, HttpOnly: true})
	values := url.Values{"client_id": {s.config.ClientID}, "client_secret": {s.config.ClientSecret}, "code": {code}, "redirect_uri": {s.config.PublicURL + "/callback"}, "code_verifier": {item.Verifier}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.config.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		http.Error(w, "GitHub request failed", 500)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "GitHub unavailable", 502)
		return
	}
	defer response.Body.Close()
	var exchange struct {
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&exchange) != nil || exchange.AccessToken == "" {
		http.Error(w, "GitHub authorization failed", 502)
		return
	}
	profileRequest, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.config.UserURL, nil)
	if err != nil {
		http.Error(w, "GitHub request failed", 500)
		return
	}
	profileRequest.Header.Set("Authorization", "Bearer "+exchange.AccessToken)
	profileRequest.Header.Set("Accept", "application/vnd.github+json")
	profile, err := s.client.Do(profileRequest)
	if err != nil {
		http.Error(w, "GitHub unavailable", 502)
		return
	}
	defer profile.Body.Close()
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if profile.StatusCode != 200 || json.NewDecoder(io.LimitReader(profile.Body, 4096)).Decode(&user) != nil || user.ID <= 0 || user.Login == "" {
		http.Error(w, "GitHub profile failed", 502)
		return
	}
	ticket, err := auth.SignBrokerTicket(s.config.SigningKey, auth.BrokerTicket{Audience: item.ReturnTo, State: item.HubState, GithubID: user.ID, Login: user.Login, Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		http.Error(w, "ticket signing failed", 500)
		return
	}
	grantCode, err := auth.Random()
	if err != nil {
		http.Error(w, "randomness unavailable", 500)
		return
	}
	s.mu.Lock()
	for id, item := range s.grants {
		if time.Now().After(item.Expires) {
			delete(s.grants, id)
		}
	}
	if len(s.grants) >= 10000 {
		s.mu.Unlock()
		http.Error(w, "too many pending logins", 429)
		return
	}
	s.grants[grantCode] = grant{Ticket: ticket, Expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	destination, _ := url.Parse(item.ReturnTo)
	destination.RawQuery = url.Values{"state": {item.HubState}, "code": {grantCode}}.Encode()
	http.Redirect(w, r, destination.String(), http.StatusFound)
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil || body.Code == "" {
		http.Error(w, "invalid exchange code", 400)
		return
	}
	s.mu.Lock()
	item, ok := s.grants[body.Code]
	delete(s.grants, body.Code)
	s.mu.Unlock()
	if !ok || time.Now().After(item.Expires) {
		http.Error(w, "expired exchange code", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": item.Ticket})
}
