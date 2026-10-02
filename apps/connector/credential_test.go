package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/devicecert"
)

// certificateHub is a minimal Hub that issues device certificates and checks proofs.
type certificateHub struct {
	t         *testing.T
	authority *devicecert.Authority
	issuedAt  time.Time
	nonces    map[string]bool
	renewals  int
	upgrades  int
}

func (h *certificateHub) issue(w http.ResponseWriter, csr, deviceID string) {
	der, _ := base64.RawURLEncoding.DecodeString(csr)
	public, err := devicecert.RequestKey(der)
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	certificate, err := h.authority.Issue(public, "space1", deviceID, h.issuedAt)
	if err != nil {
		h.t.Fatal(err)
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": deviceID, "certificate": base64.RawURLEncoding.EncodeToString(certificate), "hubUrl": "ws://" + w.(interface{ Host() string }).Host() + "/ws/v1/connectors/connect", "name": "Cert"})
}

func (h *certificateHub) proof(r *http.Request, purpose string, grace time.Duration) bool {
	deviceID, nonce, certificate, signature, ok := devicecert.ReadProof(r)
	if !ok || !h.nonces[nonce] {
		return false
	}
	delete(h.nonces, nonce)
	identity, err := h.authority.Verify(certificate, time.Now(), grace)
	return err == nil && identity.DeviceID == deviceID && devicecert.VerifyProof(identity.PublicKey, devicecert.ProofMessage(purpose, r.Host, deviceID, nonce), signature)
}

type hostWriter struct {
	http.ResponseWriter
	host string
}

func (w hostWriter) Host() string { return w.host }

func (h *certificateHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/api/v1/connectors/nonce":
		nonce := time.Now().Format(time.RFC3339Nano)
		h.nonces[nonce] = true
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": nonce})
	case "/api/v1/connectors/pair":
		h.issue(hostWriter{w, r.Host}, body["csr"], "cert-device")
	case "/api/v1/connectors/certificate":
		if r.Header.Get("Authorization") == "Bearer smd_legacy_secret" {
			h.upgrades++
			h.issue(hostWriter{w, r.Host}, body["csr"], "legacy")
			return
		}
		if !h.proof(r, devicecert.ProofRenew, devicecert.RenewGrace) {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		h.renewals++
		h.issue(hostWriter{w, r.Host}, body["csr"], r.Header.Get(devicecert.HeaderDeviceID))
	case "/ws/v1/connectors/connect":
		if !h.proof(r, devicecert.ProofConnect, 0) {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(w, r, nil)
		if err == nil {
			_ = connection.CloseNow()
		}
	default:
		http.NotFound(w, r)
	}
}

func newCertificateHub(t *testing.T) (*certificateHub, *httptest.Server) {
	certificate, key, err := devicecert.NewAuthority()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := devicecert.LoadAuthority(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	hub := &certificateHub{t: t, authority: authority, issuedAt: time.Now(), nonces: map[string]bool{}}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	return hub, server
}

func TestPairWithCertificateAndRenew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	hub, server := newCertificateHub(t)
	if err := pair([]string{"--hub", server.URL, "--code", "smp_test"}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPairedConfig()
	if err != nil || saved.DeviceToken != "" || saved.DeviceKey == "" || saved.DeviceCertificate == "" {
		t.Fatalf("certificate config: %+v %v", saved, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", info, err)
	}
	if valid, err := existingCredentialValid(saved); err != nil || !valid {
		t.Fatalf("certificate proof rejected: %v %v", valid, err)
	}
	if changed, err := refreshCredential(t.Context(), &saved); err != nil || changed {
		t.Fatalf("fresh certificate renewed early: %v %v", changed, err)
	}
	stale := saved
	key, _ := base64.RawURLEncoding.DecodeString(stale.DeviceKey)
	csr, _ := devicecert.Request(key)
	public, _ := devicecert.RequestKey(csr)
	old, _ := hub.authority.Issue(public, "space1", stale.DeviceID, time.Now().Add(-devicecert.Lifetime-time.Minute))
	stale.DeviceCertificate = base64.RawURLEncoding.EncodeToString(old)
	if valid, _ := existingCredentialValid(stale); valid {
		t.Fatal("expired certificate connected")
	}
	if changed, err := refreshCredential(t.Context(), &stale); err != nil || !changed || hub.renewals != 1 {
		t.Fatalf("expired certificate not renewed: %v %v renewals=%d", changed, err, hub.renewals)
	}
	if stale.DeviceKey != saved.DeviceKey {
		t.Fatal("renewal changed the device key")
	}
	if valid, err := existingCredentialValid(stale); err != nil || !valid {
		t.Fatalf("renewed certificate rejected: %v %v", valid, err)
	}
}

func TestLegacyTokenUpgradesToCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	hub, server := newCertificateHub(t)
	legacy := []byte(`{"deviceId":"legacy","deviceToken":"smd_legacy_secret","hubUrl":"ws://` + server.Listener.Addr().String() + `/ws/v1/connectors/connect","name":"Old"}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := pair([]string{"--hub", server.URL, "--reuse-existing"}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPairedConfig()
	if err != nil || hub.upgrades != 1 || saved.DeviceToken != "" || saved.DeviceCertificate == "" || saved.DeviceID != "legacy" {
		t.Fatalf("legacy upgrade: %+v %v upgrades=%d", saved, err, hub.upgrades)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("upgraded config permissions: %v %v", info, err)
	}
}
