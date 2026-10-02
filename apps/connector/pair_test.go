package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestPairSavesCredentialOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/v1/connectors/connect" {
			if r.Header.Get("Authorization") != "Bearer smd_device1_secret" || r.Header.Get("X-Sinthmux-Device-ID") != "device1" {
				t.Errorf("credential check did not use saved device identity")
			}
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.CloseNow()
			return
		}
		if r.URL.Path == "/api/v1/connectors/certificate" {
			http.NotFound(w, r)
			return
		}
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/connectors/pair" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != "smp_test" || body["csr"] == "" {
			t.Errorf("unexpected pairing body: %v %v", body, err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": "device1", "deviceToken": "smd_device1_secret", "hubUrl": "ws://" + r.Host + "/ws/v1/connectors/connect", "name": "My computer"})
	}))
	defer server.Close()
	args := []string{"--hub", server.URL, "--code", "smp_test"}
	if err := pair(args); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPairedConfig()
	if err != nil || saved.DeviceToken != "smd_device1_secret" || saved.Name != "My computer" {
		t.Fatalf("saved config: %+v %v", saved, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", info, err)
	}
	if err := pair(args); err == nil {
		t.Fatal("pair overwrote existing device config")
	}
	if err := pair([]string{"--hub", server.URL, "--reuse-existing"}); err != nil {
		t.Fatalf("reinstall for the same Hub: %v", err)
	}
	if err := pair([]string{"--hub", server.URL, "--code", "expired_code", "--reuse-existing"}); err != nil {
		t.Fatalf("rerun original command: %v", err)
	}
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	if err := pair([]string{"--hub", other.URL, "--reuse-existing"}); err == nil {
		t.Fatal("reinstall accepted a different Hub")
	}
	if requests != 1 {
		t.Fatalf("pair requests=%d, want 1", requests)
	}
}

func TestPairReplacesRevokedCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	var old []byte
	pairRequests := 0
	credentialStatus := http.StatusUnauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/v1/connectors/connect" {
			http.Error(w, "unavailable", credentialStatus)
			return
		}
		if r.URL.Path == "/api/v1/connectors/certificate" {
			http.NotFound(w, r)
			return
		}
		pairRequests++
		if r.URL.Path != "/api/v1/connectors/pair" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "fresh_code" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": "new", "deviceToken": "smd_new_secret", "hubUrl": "ws://" + r.Host + "/ws/v1/connectors/connect", "name": "New"})
	}))
	defer server.Close()
	old = []byte(`{"deviceId":"old","deviceToken":"smd_old_secret","hubUrl":"ws://` + server.Listener.Addr().String() + `/ws/v1/connectors/connect","name":"Old"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := pair([]string{"--hub", server.URL, "--reuse-existing"}); err == nil {
		t.Fatal("revoked credential was silently reused without pairing code")
	}
	if err := pair([]string{"--hub", server.URL, "--code", "expired_code", "--reuse-existing"}); err == nil {
		t.Fatal("expired pairing code was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(old) {
		t.Fatalf("old credential changed after rejected pairing: %v", err)
	}
	if err := pair([]string{"--hub", server.URL, "--code", "fresh_code", "--reuse-existing"}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPairedConfig()
	if err != nil || saved.DeviceID != "new" || saved.DeviceToken != "smd_new_secret" {
		t.Fatalf("replacement config: %+v %v", saved, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement permissions: %v %v", info, err)
	}
	if pairRequests != 2 {
		t.Fatalf("pair requests=%d, want 2", pairRequests)
	}
	credentialStatus = http.StatusServiceUnavailable
	if err := pair([]string{"--hub", server.URL, "--code", "another_code", "--reuse-existing"}); err == nil {
		t.Fatal("server failure was mistaken for a revoked credential")
	}
	if pairRequests != 2 {
		t.Fatalf("server failure consumed a pairing code: requests=%d", pairRequests)
	}
}

func TestPairRequiresCodeForFirstInstall(t *testing.T) {
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", filepath.Join(t.TempDir(), "connector.json"))
	if err := pair([]string{"--hub", "http://127.0.0.1:8090", "--reuse-existing"}); err == nil {
		t.Fatal("first install without a pairing code succeeded")
	}
}

func TestPairMovesToAnotherHubWithFreshCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	old := []byte(`{"DeviceID":"old","DeviceToken":"smd_old_secret","HubURL":"ws://127.0.0.1:5173/ws/v1/connectors/connect","Name":"Old"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/connectors/pair" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "fresh_code" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"deviceId": "new", "deviceToken": "smd_new_secret", "hubUrl": "ws://" + r.Host + "/ws/v1/connectors/connect", "name": "New"})
	}))
	defer server.Close()
	hub := "http://localhost:" + strings.TrimPrefix(server.Listener.Addr().String(), "127.0.0.1:")
	if err := pair([]string{"--hub", hub, "--reuse-existing"}); err == nil {
		t.Fatal("repair without a pairing code switched Hubs")
	}
	if err := pair([]string{"--hub", hub, "--code", "expired_code", "--reuse-existing"}); err == nil {
		t.Fatal("rejected pairing code was accepted")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(old) {
		t.Fatalf("old config changed after rejected pairing: %s %v", data, err)
	}
	if err := pair([]string{"--hub", hub, "--code", "fresh_code", "--reuse-existing"}); err != nil {
		t.Fatal(err)
	}
	saved, err := loadPairedConfig()
	if err != nil || saved.DeviceID != "new" || !strings.Contains(saved.HubURL, "localhost") {
		t.Fatalf("moved config: %+v %v", saved, err)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups=%v, want one", backups)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || string(data) != string(old) {
		t.Fatalf("backup content: %s %v", data, err)
	}
}
