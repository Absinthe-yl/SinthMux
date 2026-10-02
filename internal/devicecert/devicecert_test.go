package devicecert

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newAuthority(t *testing.T) *Authority {
	t.Helper()
	certificate, key, err := NewAuthority()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := LoadAuthority(certificate, key)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func issue(t *testing.T, authority *Authority, now time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := Request(key)
	if err != nil {
		t.Fatal(err)
	}
	public, err := RequestKey(csr)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := authority.Issue(public, "space1", "device1", now)
	if err != nil {
		t.Fatal(err)
	}
	return key, certificate
}

func TestIssueBindsIdentityAndKey(t *testing.T) {
	authority := newAuthority(t)
	now := time.Now()
	key, certificate := issue(t, authority, now)
	identity, err := authority.Verify(certificate, now, 0)
	if err != nil || identity.SpaceID != "space1" || identity.DeviceID != "device1" {
		t.Fatalf("identity: %+v %v", identity, err)
	}
	if _, err := Inspect(certificate, key); err != nil {
		t.Fatalf("certificate does not match its key: %v", err)
	}
	otherKey, _ := NewKey()
	if _, err := Inspect(certificate, otherKey); err == nil {
		t.Fatal("certificate accepted with another key")
	}
	if _, err := newAuthority(t).Verify(certificate, now, 0); err == nil {
		t.Fatal("certificate from another CA accepted")
	}
	parsed, _ := x509.ParseCertificate(certificate)
	if parsed.NotAfter.Sub(now) > Lifetime || len(parsed.ExtKeyUsage) != 1 || parsed.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("unexpected certificate profile: %v %v", parsed.NotAfter, parsed.ExtKeyUsage)
	}
}

func TestExpiredCertificateOnlyRenewsWithinGrace(t *testing.T) {
	authority := newAuthority(t)
	issued := time.Now().Add(-Lifetime - time.Minute)
	_, certificate := issue(t, authority, issued)
	if _, err := authority.Verify(certificate, time.Now(), 0); err == nil {
		t.Fatal("expired certificate accepted for connection")
	}
	if _, err := authority.Verify(certificate, time.Now(), RenewGrace); err != nil {
		t.Fatalf("recently expired certificate cannot renew: %v", err)
	}
	if _, err := authority.Verify(certificate, time.Now().Add(RenewGrace+Lifetime), RenewGrace); err == nil {
		t.Fatal("certificate renewed after grace period")
	}
}

func TestProofBindsPurposeAudienceAndNonce(t *testing.T) {
	authority := newAuthority(t)
	key, certificate := issue(t, authority, time.Now())
	headers := http.Header{}
	if err := SetProof(headers, ProofConnect, "Hub.Example", "device1", "nonce1", certificate, key); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header = headers
	deviceID, nonce, gotCertificate, signature, ok := ReadProof(request)
	if !ok || deviceID != "device1" || nonce != "nonce1" {
		t.Fatalf("proof headers: %v %s %s", ok, deviceID, nonce)
	}
	identity, err := authority.Verify(gotCertificate, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyProof(identity.PublicKey, ProofMessage(ProofConnect, "hub.example", "device1", "nonce1"), signature) {
		t.Fatal("valid proof rejected")
	}
	for name, message := range map[string][]byte{
		"purpose":  ProofMessage(ProofRenew, "hub.example", "device1", "nonce1"),
		"audience": ProofMessage(ProofConnect, "evil.example", "device1", "nonce1"),
		"device":   ProofMessage(ProofConnect, "hub.example", "device2", "nonce1"),
		"nonce":    ProofMessage(ProofConnect, "hub.example", "device1", "nonce2"),
	} {
		if VerifyProof(identity.PublicKey, message, signature) {
			t.Errorf("proof accepted with different %s", name)
		}
	}
}

func TestRequestRejectsInvalidCSR(t *testing.T) {
	key, _ := NewKey()
	csr, _ := Request(key)
	csr[len(csr)-1] ^= 0xff
	if _, err := RequestKey(csr); err == nil {
		t.Fatal("tampered CSR accepted")
	}
	if _, err := newAuthority(t).Issue(nil, "space1", "device1", time.Now()); err == nil {
		t.Fatal("certificate issued without key")
	}
}
