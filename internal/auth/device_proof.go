package auth

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sinthmux/sinthmux/internal/devicecert"
)

const nonceLifetime = time.Minute

// issueNonce returns a single-use challenge for a device proof.
func (s *Server) issueNonce() (string, error) {
	nonce, err := Random()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nonces) >= 10000 {
		for value, expires := range s.nonces {
			if now.After(expires) {
				delete(s.nonces, value)
			}
		}
		if len(s.nonces) >= 10000 {
			return "", ErrDenied
		}
	}
	s.nonces[nonce] = now.Add(nonceLifetime)
	return nonce, nil
}

func (s *Server) consumeNonce(nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.nonces[nonce]
	delete(s.nonces, nonce)
	return ok && time.Now().Before(expires)
}

func (s *Server) audience() string {
	public, err := url.Parse(s.OAuth.PublicURL)
	if err != nil {
		return ""
	}
	return public.Host
}

// deviceProof verifies a certificate, its signature over a fresh nonce and the
// current database binding. grace allows renewal of a recently expired certificate.
func (s *Server) deviceProof(r *http.Request, purpose string, grace time.Duration) (devicecert.Identity, bool) {
	deviceID, nonce, certificate, signature, ok := devicecert.ReadProof(r)
	if !ok || s.Authority == nil || !s.consumeNonce(nonce) {
		return devicecert.Identity{}, false
	}
	identity, err := s.Authority.Verify(certificate, time.Now(), grace)
	if err != nil || identity.DeviceID != deviceID || !devicecert.VerifyProof(identity.PublicKey, devicecert.ProofMessage(purpose, s.audience(), deviceID, nonce), signature) {
		return devicecert.Identity{}, false
	}
	return identity, s.Store.AuthenticateDeviceKey(r.Context(), identity.SpaceID, identity.DeviceID, identity.KeyHash)
}

// AuthenticateConnector returns the device allowed to open a connector WebSocket.
// Certificate proofs are preferred; legacy device tokens remain valid until the
// device first connects with its certified key.
func (s *Server) AuthenticateConnector(r *http.Request) (string, bool) {
	if r.Header.Get(devicecert.HeaderProof) != "" {
		identity, ok := s.deviceProof(r, devicecert.ProofConnect, 0)
		return identity.DeviceID, ok
	}
	deviceID := r.Header.Get(devicecert.HeaderDeviceID)
	authorization := r.Header.Get("Authorization")
	return deviceID, deviceID != "" && strings.HasPrefix(authorization, "Bearer ") && s.Store.AuthenticateDevice(r.Context(), deviceID, strings.TrimPrefix(authorization, "Bearer "))
}

func (s *Server) deviceNonce(w http.ResponseWriter, r *http.Request) {
	if s.Authority == nil {
		respond(w, 404, map[string]string{"error": "device certificates unavailable"})
		return
	}
	// Devices behind one reverse proxy share a remote address, so the nonce pool
	// is bounded globally instead of rate limited per address.
	nonce, err := s.issueNonce()
	if err != nil {
		respond(w, 503, map[string]string{"error": "nonce unavailable"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]string{"nonce": nonce})
}

// renewDeviceCertificate reissues a certificate for the same key, or upgrades a
// legacy token device to a certified key.
func (s *Server) renewDeviceCertificate(w http.ResponseWriter, r *http.Request) {
	if s.Authority == nil {
		respond(w, 404, map[string]string{"error": "device certificates unavailable"})
		return
	}
	if !s.rateLimit("renew:" + r.Header.Get(devicecert.HeaderDeviceID)) {
		respond(w, 429, map[string]string{"error": "too many attempts"})
		return
	}
	var body struct {
		CSR string `json:"csr"`
	}
	if !readBody(w, r, &body) {
		return
	}
	public, err := csrKey(body.CSR)
	if err != nil {
		respond(w, 400, map[string]string{"error": "invalid certificate request"})
		return
	}
	var device Device
	action := "device.certificate.renew"
	if r.Header.Get(devicecert.HeaderProof) != "" {
		identity, ok := s.deviceProof(r, devicecert.ProofRenew, devicecert.RenewGrace)
		if !ok || string(identity.KeyHash) != string(devicecert.KeyHash(public)) {
			respond(w, 401, map[string]string{"error": "invalid device credential"})
			return
		}
		device = Device{ID: identity.DeviceID, SpaceID: identity.SpaceID}
	} else {
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer {
			device, err = s.Store.BindDeviceKey(r.Context(), r.Header.Get(devicecert.HeaderDeviceID), token, devicecert.KeyHash(public))
		}
		if !bearer || err != nil {
			respond(w, 401, map[string]string{"error": "invalid device credential"})
			return
		}
		action = "device.certificate.upgrade"
	}
	certificate, err := s.Authority.Issue(public, device.SpaceID, device.ID, time.Now())
	if err != nil {
		respond(w, 500, map[string]string{"error": "certificate unavailable"})
		return
	}
	s.Store.Audit(r.Context(), "", device.SpaceID, device.ID, action, "ok")
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 201, map[string]string{"certificate": base64.RawURLEncoding.EncodeToString(certificate)})
}

func csrKey(encoded string) (*ecdsa.PublicKey, error) {
	der, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, devicecert.ErrInvalid
	}
	return devicecert.RequestKey(der)
}

// LoadDeviceAuthority enables certificate pairing and proofs.
func (s *Server) LoadDeviceAuthority(ctx context.Context) error {
	authority, err := s.Store.DeviceAuthority(ctx)
	if err == nil {
		s.Authority = authority
	}
	return err
}
