package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type BrokerTicket struct {
	Audience string `json:"aud"`
	State    string `json:"state"`
	GithubID int64  `json:"githubId"`
	Login    string `json:"login"`
	Expires  int64  `json:"exp"`
}

func SignBrokerTicket(key ed25519.PrivateKey, ticket BrokerTicket) (string, error) {
	if len(key) != ed25519.PrivateKeySize || ticket.Audience == "" || ticket.State == "" || ticket.GithubID <= 0 || ticket.Login == "" || ticket.Expires <= time.Now().Unix() {
		return "", errors.New("invalid broker ticket")
	}
	payload, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyBrokerTicket(key ed25519.PublicKey, raw, audience, state string) (BrokerTicket, error) {
	parts := strings.Split(raw, ".")
	if len(key) != ed25519.PublicKeySize || len(parts) != 2 || len(raw) > 4096 {
		return BrokerTicket{}, errors.New("invalid broker ticket")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return BrokerTicket{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !ed25519.Verify(key, payload, signature) {
		return BrokerTicket{}, errors.New("invalid broker signature")
	}
	var ticket BrokerTicket
	if json.Unmarshal(payload, &ticket) != nil || ticket.Audience != audience || ticket.State != state || ticket.GithubID <= 0 || ticket.Login == "" || ticket.Expires <= time.Now().Unix() || ticket.Expires > time.Now().Add(2*time.Minute).Unix() {
		return BrokerTicket{}, errors.New("expired or mismatched broker ticket")
	}
	return ticket, nil
}
