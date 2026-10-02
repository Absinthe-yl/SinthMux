package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/sinthmux/sinthmux/internal/config"
	"github.com/sinthmux/sinthmux/internal/devicecert"
)

// renewBefore starts renewal once a third of the certificate lifetime remains.
const renewBefore = devicecert.Lifetime / 3

var errCredentialRejected = errors.New("Hub 拒绝了设备凭据")

var httpClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

// connectHeaders returns the WebSocket handshake headers for the current credential.
func connectHeaders(ctx context.Context, settings config.Connector) (http.Header, error) {
	headers := http.Header{}
	switch {
	case settings.DeviceKey != "":
		key, certificate, err := decodeCredential(settings)
		if err != nil {
			return nil, err
		}
		nonce, err := fetchNonce(ctx, settings.HubURL)
		if err != nil {
			return nil, err
		}
		err = devicecert.SetProof(headers, devicecert.ProofConnect, audience(settings.HubURL), settings.DeviceID, nonce, certificate, key)
		return headers, err
	case settings.DeviceToken != "":
		headers.Set("Authorization", "Bearer "+settings.DeviceToken)
		headers.Set(devicecert.HeaderDeviceID, settings.DeviceID)
	default:
		headers.Set("Authorization", "Bearer "+settings.DevToken)
	}
	return headers, nil
}

// refreshCredential upgrades a legacy device token to a certified key, or renews
// a certificate close to expiry. It returns true when settings changed.
func refreshCredential(ctx context.Context, settings *config.Connector) (bool, error) {
	if settings.DeviceKey == "" {
		if settings.DeviceToken == "" {
			return false, nil
		}
		key, err := devicecert.NewKey()
		if err != nil {
			return false, err
		}
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+settings.DeviceToken)
		headers.Set(devicecert.HeaderDeviceID, settings.DeviceID)
		certificate, err := requestCertificate(ctx, settings.HubURL, key, headers)
		if errors.Is(err, errCertificatesUnavailable) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := acceptCertificate(settings, key, certificate); err != nil {
			return false, err
		}
		settings.DeviceToken = ""
		return true, nil
	}
	key, certificate, err := decodeCredential(*settings)
	if err != nil {
		return false, err
	}
	identity, err := devicecert.Inspect(certificate, key)
	if err != nil {
		return false, err
	}
	if time.Until(identity.NotAfter) > renewBefore {
		return false, nil
	}
	nonce, err := fetchNonce(ctx, settings.HubURL)
	if err != nil {
		return false, err
	}
	headers := http.Header{}
	if err := devicecert.SetProof(headers, devicecert.ProofRenew, audience(settings.HubURL), settings.DeviceID, nonce, certificate, key); err != nil {
		return false, err
	}
	renewed, err := requestCertificate(ctx, settings.HubURL, key, headers)
	if err != nil {
		return false, err
	}
	return true, acceptCertificate(settings, key, renewed)
}

func acceptCertificate(settings *config.Connector, key, certificate []byte) error {
	identity, err := devicecert.Inspect(certificate, key)
	if err != nil || identity.DeviceID != settings.DeviceID {
		return errors.New("Hub 返回的设备证书无效")
	}
	settings.DeviceKey = base64.RawURLEncoding.EncodeToString(key)
	settings.DeviceCertificate = base64.RawURLEncoding.EncodeToString(certificate)
	return nil
}

func decodeCredential(settings config.Connector) ([]byte, []byte, error) {
	key, keyErr := base64.RawURLEncoding.DecodeString(settings.DeviceKey)
	certificate, certErr := base64.RawURLEncoding.DecodeString(settings.DeviceCertificate)
	if keyErr != nil || certErr != nil || len(certificate) == 0 {
		return nil, nil, errors.New("设备证书配置无效")
	}
	return key, certificate, nil
}

var errCertificatesUnavailable = errors.New("Hub 不支持设备证书")

func requestCertificate(ctx context.Context, hubURL string, key []byte, headers http.Header) ([]byte, error) {
	csr, err := devicecert.Request(key)
	if err != nil {
		return nil, err
	}
	var result struct{ Certificate string }
	status, err := postJSON(ctx, apiURL(hubURL, "/api/v1/connectors/certificate"), map[string]string{"csr": base64.RawURLEncoding.EncodeToString(csr)}, headers, &result)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusCreated:
	case http.StatusNotFound:
		return nil, errCertificatesUnavailable
	case http.StatusUnauthorized:
		return nil, errCredentialRejected
	default:
		return nil, fmt.Errorf("申请设备证书失败（HTTP %d）", status)
	}
	return base64.RawURLEncoding.DecodeString(result.Certificate)
}

func fetchNonce(ctx context.Context, hubURL string) (string, error) {
	var result struct{ Nonce string }
	status, err := postJSON(ctx, apiURL(hubURL, "/api/v1/connectors/nonce"), nil, nil, &result)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || result.Nonce == "" {
		return "", fmt.Errorf("获取设备认证挑战失败（HTTP %d）", status)
	}
	return result.Nonce, nil
}

func postJSON(ctx context.Context, endpoint string, body any, headers http.Header, out any) (int, error) {
	payload := []byte("{}")
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	for name, values := range headers {
		request.Header[name] = values
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(out); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

// apiURL maps the connector WebSocket URL to an HTTP endpoint on the same Hub.
func apiURL(hubURL, path string) string {
	endpoint, err := url.Parse(hubURL)
	if err != nil {
		return ""
	}
	if endpoint.Scheme == "wss" {
		endpoint.Scheme = "https"
	} else {
		endpoint.Scheme = "http"
	}
	endpoint.Path, endpoint.RawQuery = path, ""
	return endpoint.String()
}

func audience(hubURL string) string {
	endpoint, err := url.Parse(hubURL)
	if err != nil {
		return ""
	}
	return endpoint.Host
}
