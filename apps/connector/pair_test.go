package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPairSavesCredentialOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connector.json")
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", path)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/connectors/pair" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != "smp_test" {
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

func TestPairRequiresCodeForFirstInstall(t *testing.T) {
	t.Setenv("SINTHMUX_CONNECTOR_CONFIG", filepath.Join(t.TempDir(), "connector.json"))
	if err := pair([]string{"--hub", "http://127.0.0.1:8090", "--reuse-existing"}); err == nil {
		t.Fatal("first install without a pairing code succeeded")
	}
}
