package protocol

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type (
	Credentials struct {
		SensorID string
		Secret   string
	}

	APIError struct {
		StatusCode int
		Code       string
		Detail     string
		RetryAfter time.Duration
	}

	Client struct {
		HTTP *http.Client
		Now  func() time.Time
	}
)

const (
	maxResponseBytes = 1 << 20
)

var (
	ErrRevoked = errors.New("sensor credentials revoked")

	errRedirectRejected = errors.New("controller redirects are not allowed")
)

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("controller returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Detail)
	}
	return fmt.Sprintf("controller returned HTTP %d: %s", e.StatusCode, e.Detail)
}

func NewClient() *Client {
	// The assertion can only fail if something replaced http.DefaultTransport;
	// fall back to a fresh transport in that case.
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.MaxIdleConns = 4
	transport.MaxIdleConnsPerHost = 2
	transport.IdleConnTimeout = 90 * time.Second
	return &Client{
		HTTP: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: rejectRedirect},
		Now:  time.Now,
	}
}

func (c *Client) Enroll(ctx context.Context, controllerURL string, request EnrollmentRequest) (EnrollmentResponse, error) {
	endpoint, err := endpointURL(controllerURL, EnrollPath)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return EnrollmentResponse{}, fmt.Errorf("encode enrollment request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return EnrollmentResponse{}, fmt.Errorf("create enrollment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return EnrollmentResponse{}, fmt.Errorf("enroll sensor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return EnrollmentResponse{}, decodeAPIError(resp)
	}
	var result EnrollmentResponse
	if err := decodeJSON(resp.Body, &result); err != nil {
		return EnrollmentResponse{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	if result.SensorID == "" || result.SensorSecret == "" || result.IngestURL == "" || result.ConfigVersion < 1 {
		return EnrollmentResponse{}, errors.New("controller returned incomplete enrollment credentials")
	}
	if err := ValidateIngestURL(controllerURL, result.IngestURL); err != nil {
		return EnrollmentResponse{}, fmt.Errorf("invalid ingest URL: %w", err)
	}
	result.Config = NormalizeConfig(result.Config)
	return result, nil
}

func (c *Client) Ingest(ctx context.Context, ingestURL string, credentials Credentials, compressedBody []byte) (IngestResponse, error) {
	parsed, err := validateHTTPSURL(ingestURL)
	if err != nil {
		return IngestResponse{}, err
	}
	var result IngestResponse
	err = c.doSigned(ctx, http.MethodPost, parsed, credentials, compressedBody, "application/json", "zstd", &result)
	return result, err
}

func (c *Client) Config(ctx context.Context, controllerURL string, credentials Credentials) (ConfigResponse, error) {
	endpoint, err := endpointURL(controllerURL, ConfigPath)
	if err != nil {
		return ConfigResponse{}, err
	}
	parsed, _ := url.Parse(endpoint)
	var result ConfigResponse
	if err := c.doSigned(ctx, http.MethodGet, parsed, credentials, nil, "", "", &result); err != nil {
		return ConfigResponse{}, err
	}
	result.Config = NormalizeConfig(result.Config)
	return result, nil
}

func (c *Client) Heartbeat(ctx context.Context, controllerURL string, credentials Credentials, heartbeat HeartbeatRequest) (HeartbeatResponse, error) {
	endpoint, err := endpointURL(controllerURL, HeartbeatPath)
	if err != nil {
		return HeartbeatResponse{}, err
	}
	body, err := json.Marshal(heartbeat)
	if err != nil {
		return HeartbeatResponse{}, fmt.Errorf("encode heartbeat: %w", err)
	}
	parsed, _ := url.Parse(endpoint)
	var result HeartbeatResponse
	err = c.doSigned(ctx, http.MethodPost, parsed, credentials, body, "application/json", "", &result)
	return result, err
}

func (c *Client) doSigned(ctx context.Context, method string, endpoint *url.URL, credentials Credentials, body []byte, contentType, contentEncoding string, out any) error {
	if credentials.SensorID == "" || credentials.Secret == "" {
		return errors.New("missing sensor credentials")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create signed request: %w", err)
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate request nonce: %w", err)
	}
	nonce := base64.StdEncoding.EncodeToString(nonceBytes)
	path := endpoint.EscapedPath()
	if path == "" {
		path = "/"
	}
	signature, err := Sign(method, path, timestamp, nonce, body, credentials.Secret)
	if err != nil {
		return err
	}
	req.Header.Set("X-Axon-Sensor-Id", credentials.SensorID)
	req.Header.Set("X-Axon-Sensor-Proto", "1")
	req.Header.Set("X-Axon-Timestamp", timestamp)
	req.Header.Set("X-Axon-Nonce", nonce)
	req.Header.Set("X-Axon-Signature", signature)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("send signed request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := decodeAPIError(resp)
		if resp.StatusCode == http.StatusUnauthorized && apiErr.Code == "sensor_revoked" {
			return fmt.Errorf("%w: %s", ErrRevoked, apiErr.Detail)
		}
		return apiErr
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := decodeJSON(resp.Body, out); err != nil {
		return fmt.Errorf("decode controller response: %w", err)
	}
	return nil
}

func Sign(method, path, timestamp, nonce string, body []byte, encodedSecret string) (string, error) {
	secret, err := decodeSensorSecret(encodedSecret)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	canonical := strings.Join([]string{
		strings.ToUpper(method),
		path,
		timestamp,
		nonce,
		hex.EncodeToString(digest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(canonical))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeSensorSecret(encoded string) ([]byte, error) {
	secret, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		secret, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(secret) < 16 {
		return nil, errors.New("invalid sensor secret")
	}
	return secret, nil
}

func endpointURL(controllerURL, path string) (string, error) {
	base, err := validateHTTPSURL(controllerURL)
	if err != nil {
		return "", err
	}
	base.Path = path
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func validateHTTPSURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("controller URL must be an absolute URL")
	}
	if parsed.User != nil {
		return nil, errors.New("controller URL must not include user information")
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("controller URL must use HTTPS")
	}
	return parsed, nil
}

// ValidateControllerURL rejects controller and ingest endpoints that could
// expose claim tokens or sensor credentials over plaintext transport.
func ValidateControllerURL(raw string) error {
	_, err := validateHTTPSURL(raw)
	return err
}

// ValidateIngestURL confines controller-provided ingest paths to the origin
// the user explicitly trusted. This permits private/on-prem controllers while
// preventing a controller response from turning the sensor into an SSRF
// client for an unrelated internal service.
func ValidateIngestURL(controllerURL, ingestURL string) error {
	controller, err := validateHTTPSURL(controllerURL)
	if err != nil {
		return err
	}
	ingest, err := validateHTTPSURL(ingestURL)
	if err != nil {
		return err
	}
	if !strings.EqualFold(controller.Hostname(), ingest.Hostname()) || effectiveHTTPSPort(controller) != effectiveHTTPSPort(ingest) {
		return errors.New("ingest URL must use the enrolled controller origin")
	}
	return nil
}

func effectiveHTTPSPort(endpoint *url.URL) string {
	if endpoint.Port() == "" {
		return "443"
	}
	return endpoint.Port()
}

func decodeJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseBytes+1))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	return nil
}

func decodeAPIError(resp *http.Response) *APIError {
	payload := struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}{}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload)
	if payload.Detail == "" {
		payload.Detail = http.StatusText(resp.StatusCode)
	}
	return &APIError{
		StatusCode: resp.StatusCode,
		Code:       payload.Code,
		Detail:     payload.Detail,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NewClient().HTTP
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	configured := c.httpClient()
	client := &http.Client{
		Transport:     configured.Transport,
		CheckRedirect: rejectRedirect,
		Jar:           configured.Jar,
		Timeout:       configured.Timeout,
	}
	return client.Do(request)
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return errRedirectRejected
}

// NewBatchID returns an RFC 9562 UUIDv7 using wall-clock milliseconds and
// cryptographically random remaining bits.
func NewBatchID(now time.Time) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate batch ID: %w", err)
	}
	millis := uint64(now.UnixMilli())
	value[0] = byte(millis >> 40)
	value[1] = byte(millis >> 32)
	value[2] = byte(millis >> 24)
	value[3] = byte(millis >> 16)
	value[4] = byte(millis >> 8)
	value[5] = byte(millis)
	value[6] = (value[6] & 0x0f) | 0x70
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
