package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/sinthmux/sinthmux/internal/devicecert"
	"github.com/sinthmux/sinthmux/internal/devices"
	"github.com/sinthmux/sinthmux/internal/relay"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestDeviceCertificateIntegration(t *testing.T) {
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
	owner, err := s.GithubUser(ctx, time.Now().UnixNano(), "cert-owner")
	if err != nil {
		t.Fatal(err)
	}
	spaces, err := s.Spaces(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	spaceID := spaces[0].ID
	server := NewServer(s, OAuthConfig{PublicURL: "http://127.0.0.1:5173"})
	if err := server.LoadDeviceAuthority(ctx); err != nil {
		t.Fatal(err)
	}
	again := NewServer(s, OAuthConfig{PublicURL: "http://127.0.0.1:5173"})
	if err := again.LoadDeviceAuthority(ctx); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	server.Mount(router)
	manager := relay.NewManager()
	router.Handle("/ws/v1/connectors/connect", relay.ConnectorHandler{Registry: devices.NewRegistry(), Manager: manager, AuthenticateDevice: server.AuthenticateConnector})
	hub := httptest.NewServer(router)
	defer hub.Close()
	post := func(path string, body any, headers http.Header) (int, map[string]string) {
		payload, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, hub.URL+path, strings.NewReader(string(payload)))
		for name, values := range headers {
			request.Header[name] = values
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result := map[string]string{}
		_ = json.NewDecoder(response.Body).Decode(&result)
		return response.StatusCode, result
	}
	nonce := func() string {
		status, body := post("/api/v1/connectors/nonce", nil, nil)
		if status != 200 || body["nonce"] == "" {
			t.Fatalf("nonce status=%d", status)
		}
		return body["nonce"]
	}
	csr := func(key []byte) string {
		request, err := devicecert.Request(key)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(request)
	}
	proof := func(purpose, deviceID, value string, certificate, key []byte) http.Header {
		headers := http.Header{}
		if err := devicecert.SetProof(headers, purpose, "127.0.0.1:5173", deviceID, value, certificate, key); err != nil {
			t.Fatal(err)
		}
		return headers
	}
	dial := func(headers http.Header) (*websocket.Conn, int) {
		connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hub.URL, "http")+"/ws/v1/connectors/connect", &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			if response == nil {
				t.Fatal(err)
			}
			return nil, response.StatusCode
		}
		t.Cleanup(func() { connection.CloseNow() })
		return connection, 101
	}

	key, _ := devicecert.NewKey()
	pairing, _, err := s.NewDevicePairing(ctx, spaceID, owner.ID, "cert-device")
	if err != nil {
		t.Fatal(err)
	}
	status, paired := post("/api/v1/connectors/pair", map[string]string{"code": pairing, "csr": csr(key)}, nil)
	if status != 201 || paired["certificate"] == "" || paired["deviceToken"] != "" {
		t.Fatalf("certificate pairing status=%d body=%v", status, paired)
	}
	deviceID := paired["deviceId"]
	certificate, _ := base64.RawURLEncoding.DecodeString(paired["certificate"])
	if identity, err := devicecert.Inspect(certificate, key); err != nil || identity.SpaceID != spaceID || identity.DeviceID != deviceID {
		t.Fatalf("issued identity: %+v %v", identity, err)
	}

	connection, code := dial(proof(devicecert.ProofConnect, deviceID, nonce(), certificate, key))
	if code != 101 {
		t.Fatalf("certified connector rejected: %d", code)
	}
	hello, _ := json.Marshal(protocol.Envelope{Version: protocol.Version, Type: protocol.MessageConnectorHello, Hello: &protocol.ConnectorHello{DeviceID: deviceID}})
	_ = connection.Write(ctx, websocket.MessageText, hello)
	if _, _, err := connection.Read(ctx); err != nil || !manager.Online(deviceID) {
		t.Fatalf("certified connector not registered: %v", err)
	}
	connection.CloseNow()

	used := nonce()
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, used, certificate, key)); code != 101 {
		t.Fatalf("fresh nonce rejected: %d", code)
	}
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, used, certificate, key)); code != 401 {
		t.Fatalf("replayed nonce status=%d", code)
	}
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, "made-up", certificate, key)); code != 401 {
		t.Fatalf("unknown nonce status=%d", code)
	}
	stolenKey, _ := devicecert.NewKey()
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, nonce(), certificate, stolenKey)); code != 401 {
		t.Fatalf("certificate without its key status=%d", code)
	}
	if _, code := dial(proof(devicecert.ProofRenew, deviceID, nonce(), certificate, key)); code != 401 {
		t.Fatalf("renew proof accepted for connection: %d", code)
	}
	wrongAudience := http.Header{}
	_ = devicecert.SetProof(wrongAudience, devicecert.ProofConnect, "evil.example", deviceID, nonce(), certificate, key)
	if _, code := dial(wrongAudience); code != 401 {
		t.Fatalf("proof for another Hub status=%d", code)
	}
	otherAuthority, otherKey, _ := devicecert.NewAuthority()
	forgedCA, _ := devicecert.LoadAuthority(otherAuthority, otherKey)
	public, _ := devicecert.RequestKey(mustDecode(t, csr(key)))
	forged, _ := forgedCA.Issue(public, spaceID, deviceID, time.Now())
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, nonce(), forged, key)); code != 401 {
		t.Fatalf("certificate from foreign CA status=%d", code)
	}

	expired, _ := server.Authority.Issue(public, spaceID, deviceID, time.Now().Add(-devicecert.Lifetime-time.Minute))
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, nonce(), expired, key)); code != 401 {
		t.Fatalf("expired certificate connected: %d", code)
	}
	status, renewed := post("/api/v1/connectors/certificate", map[string]string{"csr": csr(key)}, proof(devicecert.ProofRenew, deviceID, nonce(), expired, key))
	if status != 201 || renewed["certificate"] == "" {
		t.Fatalf("renew within grace status=%d", status)
	}
	newKey, _ := devicecert.NewKey()
	if status, _ := post("/api/v1/connectors/certificate", map[string]string{"csr": csr(newKey)}, proof(devicecert.ProofRenew, deviceID, nonce(), certificate, key)); status != 401 {
		t.Fatalf("renewal swapped key without re-pairing: %d", status)
	}

	legacyPairing, _, _ := s.NewDevicePairing(ctx, spaceID, owner.ID, "legacy-device")
	legacy, token, err := s.RedeemDevicePairing(ctx, legacyPairing, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyHeaders := http.Header{"Authorization": {"Bearer " + token}, devicecert.HeaderDeviceID: {legacy.ID}}
	if _, code := dial(legacyHeaders); code != 101 {
		t.Fatalf("legacy token rejected before upgrade: %d", code)
	}
	upgradeKey, _ := devicecert.NewKey()
	status, upgraded := post("/api/v1/connectors/certificate", map[string]string{"csr": csr(upgradeKey)}, legacyHeaders)
	if status != 201 {
		t.Fatalf("legacy upgrade status=%d", status)
	}
	upgradedCertificate, _ := base64.RawURLEncoding.DecodeString(upgraded["certificate"])
	if _, code := dial(legacyHeaders); code != 101 {
		t.Fatalf("legacy token stopped before first certified connection: %d", code)
	}
	if _, code := dial(proof(devicecert.ProofConnect, legacy.ID, nonce(), upgradedCertificate, upgradeKey)); code != 101 {
		t.Fatalf("upgraded certificate rejected: %d", code)
	}
	if _, code := dial(legacyHeaders); code != 401 {
		t.Fatalf("legacy token still works after certified connection: %d", code)
	}

	if err := s.RevokeDevice(ctx, spaceID, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, code := dial(proof(devicecert.ProofConnect, deviceID, nonce(), certificate, key)); code != 401 {
		t.Fatalf("revoked device connected: %d", code)
	}
	if status, _ := post("/api/v1/connectors/certificate", map[string]string{"csr": csr(key)}, proof(devicecert.ProofRenew, deviceID, nonce(), certificate, key)); status != 401 {
		t.Fatalf("revoked device renewed: %d", status)
	}
}

func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
