// Package devicecert issues short-lived X.509 device certificates and checks
// application-layer proofs that a connector holds the certified private key.
// The proof travels in WebSocket handshake headers, so it survives TLS
// termination at Nginx or Caddy while keeping the private key on the device.
package devicecert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	Lifetime     = 24 * time.Hour
	RenewGrace   = 30 * 24 * time.Hour
	ProofConnect = "connect"
	ProofRenew   = "renew"

	HeaderDeviceID    = "X-Sinthmux-Device-ID"
	HeaderNonce       = "X-Sinthmux-Device-Nonce"
	HeaderCertificate = "X-Sinthmux-Device-Certificate"
	HeaderProof       = "X-Sinthmux-Device-Proof"
)

var ErrInvalid = errors.New("invalid device certificate")

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Identity struct {
	SpaceID   string
	DeviceID  string
	PublicKey *ecdsa.PublicKey
	KeyHash   []byte
	NotAfter  time.Time
}

type Authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	roots       *x509.CertPool
}

// NewAuthority creates a P-256 device CA and returns its certificate and PKCS#8 key.
func NewAuthority() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "SinthMux Device CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	return certificate, keyDER, err
}

func LoadAuthority(certificateDER, keyDER []byte) (*Authority, error) {
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(keyDER)
	if err != nil {
		return nil, err
	}
	if !certificate.IsCA || !key.PublicKey.Equal(certificate.PublicKey) {
		return nil, errors.New("device CA certificate and key do not match")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &Authority{certificate: certificate, key: key, roots: roots}, nil
}

// Issue binds a device public key to its space and device ID through a URI SAN.
func (a *Authority) Issue(public *ecdsa.PublicKey, spaceID, deviceID string, now time.Time) ([]byte, error) {
	if !validKey(public) || !idPattern.MatchString(spaceID) || !idPattern.MatchString(deviceID) {
		return nil, ErrInvalid
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: deviceID}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(Lifetime), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{{Scheme: "sinthmux", Host: "device", Path: "/" + spaceID + "/" + deviceID}}}
	return x509.CreateCertificate(rand.Reader, template, a.certificate, public, a.key)
}

// Verify checks the CA signature and validity. A positive grace accepts a
// recently expired certificate, which is used only for renewal.
func (a *Authority) Verify(certificateDER []byte, now time.Time, grace time.Duration) (Identity, error) {
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return Identity{}, ErrInvalid
	}
	at := now
	if grace > 0 && now.After(certificate.NotAfter) && now.Sub(certificate.NotAfter) <= grace {
		at = certificate.NotAfter
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: a.roots, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return Identity{}, ErrInvalid
	}
	return identityOf(certificate)
}

// Inspect is used by the connector, which does not hold the CA, to check that
// a certificate returned by the Hub matches its local key.
func Inspect(certificateDER, keyDER []byte) (Identity, error) {
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		return Identity{}, ErrInvalid
	}
	key, err := parseKey(keyDER)
	if err != nil {
		return Identity{}, err
	}
	identity, err := identityOf(certificate)
	if err != nil || !identity.PublicKey.Equal(&key.PublicKey) {
		return Identity{}, ErrInvalid
	}
	return identity, nil
}

func NewKey() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKCS8PrivateKey(key)
}

func Request(keyDER []byte) ([]byte, error) {
	key, err := parseKey(keyDER)
	if err != nil {
		return nil, err
	}
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "sinthmux-connector"}}, key)
}

// RequestKey returns the CSR public key after checking the CSR self-signature.
// Subject fields are ignored: the Hub decides the device identity.
func RequestKey(csrDER []byte) (*ecdsa.PublicKey, error) {
	request, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || request.CheckSignature() != nil {
		return nil, ErrInvalid
	}
	public, ok := request.PublicKey.(*ecdsa.PublicKey)
	if !ok || !validKey(public) {
		return nil, ErrInvalid
	}
	return public, nil
}

func KeyHash(public *ecdsa.PublicKey) []byte {
	der, _ := x509.MarshalPKIXPublicKey(public)
	sum := sha256.Sum256(der)
	return sum[:]
}

// ProofMessage binds a signature to its purpose, the Hub host, the device and a single-use nonce.
func ProofMessage(purpose, audience, deviceID, nonce string) []byte {
	return []byte("sinthmux-device-proof-v1\n" + purpose + "\n" + strings.ToLower(audience) + "\n" + deviceID + "\n" + nonce)
}

func SetProof(headers http.Header, purpose, audience, deviceID, nonce string, certificateDER, keyDER []byte) error {
	key, err := parseKey(keyDER)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(ProofMessage(purpose, audience, deviceID, nonce))
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return err
	}
	headers.Set(HeaderDeviceID, deviceID)
	headers.Set(HeaderNonce, nonce)
	headers.Set(HeaderCertificate, base64.RawURLEncoding.EncodeToString(certificateDER))
	headers.Set(HeaderProof, base64.RawURLEncoding.EncodeToString(signature))
	return nil
}

func ReadProof(r *http.Request) (deviceID, nonce string, certificateDER, signature []byte, ok bool) {
	deviceID, nonce = r.Header.Get(HeaderDeviceID), r.Header.Get(HeaderNonce)
	certificateDER, certErr := base64.RawURLEncoding.DecodeString(r.Header.Get(HeaderCertificate))
	signature, proofErr := base64.RawURLEncoding.DecodeString(r.Header.Get(HeaderProof))
	ok = deviceID != "" && nonce != "" && certErr == nil && proofErr == nil && len(certificateDER) > 0 && len(certificateDER) < 4096 && len(signature) > 0 && len(signature) < 256
	return
}

func VerifyProof(public *ecdsa.PublicKey, message, signature []byte) bool {
	sum := sha256.Sum256(message)
	return ecdsa.VerifyASN1(public, sum[:], signature)
}

func identityOf(certificate *x509.Certificate) (Identity, error) {
	public, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || !validKey(public) || len(certificate.URIs) != 1 {
		return Identity{}, ErrInvalid
	}
	uri := certificate.URIs[0]
	parts := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
	if uri.Scheme != "sinthmux" || uri.Host != "device" || len(parts) != 2 || !idPattern.MatchString(parts[0]) || !idPattern.MatchString(parts[1]) || certificate.Subject.CommonName != parts[1] {
		return Identity{}, ErrInvalid
	}
	return Identity{SpaceID: parts[0], DeviceID: parts[1], PublicKey: public, KeyHash: KeyHash(public), NotAfter: certificate.NotAfter}, nil
}

func parseKey(keyDER []byte) (*ecdsa.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !validKey(&key.PublicKey) {
		return nil, ErrInvalid
	}
	return key, nil
}

func validKey(public *ecdsa.PublicKey) bool {
	return public != nil && public.Curve == elliptic.P256()
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
