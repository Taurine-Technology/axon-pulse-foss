package protocol

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignMatchesCanonicalContract(t *testing.T) {
	t.Parallel()
	secret := []byte("01234567890123456789012345678901")
	body := []byte(`{"ok":true}`)
	got, err := Sign("post", IngestPath, "1700000000", "nonce", body, base64.StdEncoding.EncodeToString(secret))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	canonical := "POST\n" + IngestPath + "\n1700000000\nnonce\n" + hex.EncodeToString(digest[:])
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(canonical))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestNewBatchIDIsUUIDv7(t *testing.T) {
	t.Parallel()
	id, err := NewBatchID(time.UnixMilli(1_700_000_000_123))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("invalid UUIDv7 %q", id)
	}
}

func TestSignedIngestCoversCompressedBody(t *testing.T) {
	t.Parallel()
	secret := []byte("01234567890123456789012345678901")
	body := []byte("zstd bytes")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ := io.ReadAll(r.Body)
		if string(gotBody) != string(body) || r.Header.Get("Content-Encoding") != "zstd" {
			t.Errorf("unexpected encoded request")
		}
		nonce, err := base64.StdEncoding.Strict().DecodeString(r.Header.Get("X-Axon-Nonce"))
		if err != nil || len(nonce) != 16 {
			t.Errorf("nonce is not padded standard base64 for 16 bytes: %q", r.Header.Get("X-Axon-Nonce"))
		}
		want, _ := Sign(r.Method, r.URL.EscapedPath(), r.Header.Get("X-Axon-Timestamp"), r.Header.Get("X-Axon-Nonce"), gotBody, base64.StdEncoding.EncodeToString(secret))
		if !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Axon-Signature"))) {
			t.Error("signature does not cover wire body")
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(IngestResponse{ConfigVersion: 2})
	}))
	defer server.Close()

	client := NewClient()
	client.HTTP = server.Client()
	result, err := client.Ingest(context.Background(), server.URL+IngestPath, Credentials{SensorID: "id", Secret: base64.StdEncoding.EncodeToString(secret)}, body)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigVersion != 2 {
		t.Fatalf("config version = %d", result.ConfigVersion)
	}
}

func TestSignedIngestUsesRootPathForHostOnlyURL(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		want, _ := Sign(r.Method, "/", r.Header.Get("X-Axon-Timestamp"), r.Header.Get("X-Axon-Nonce"), body, secret)
		if r.URL.EscapedPath() != "/" || !hmac.Equal([]byte(want), []byte(r.Header.Get("X-Axon-Signature"))) {
			t.Errorf("request path = %q or signature did not cover root path", r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(IngestResponse{ConfigVersion: 1})
	}))
	defer server.Close()

	client := NewClient()
	client.HTTP = server.Client()
	if _, err := client.Ingest(context.Background(), server.URL, Credentials{SensorID: "id", Secret: secret}, []byte("body")); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedResponseIsClassified(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"sensor_revoked","detail":"revoked"}`)
	}))
	defer server.Close()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	client := NewClient()
	client.HTTP = server.Client()
	_, err := client.Ingest(context.Background(), server.URL+IngestPath, Credentials{SensorID: "id", Secret: secret}, []byte("body"))
	if err == nil || !strings.Contains(err.Error(), ErrRevoked.Error()) {
		t.Fatalf("error = %v, want revoked", err)
	}
}

func TestCredentialBearingRequestsRejectRedirects(t *testing.T) {
	t.Parallel()
	var redirectedRequests atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client := NewClient()
	client.HTTP = redirector.Client()
	_, err := client.Enroll(context.Background(), redirector.URL, EnrollmentRequest{Token: "secret"})
	if !errors.Is(err, errRedirectRejected) {
		t.Fatalf("Enroll redirect error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("enrollment token was sent to the redirect target")
	}

	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	_, err = client.Ingest(context.Background(), redirector.URL, Credentials{SensorID: "id", Secret: secret}, []byte("signed"))
	if !errors.Is(err, errRedirectRejected) {
		t.Fatalf("signed redirect error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatal("signed credentials were sent to the redirect target")
	}
}

func TestControllerResponsesAllowAdditiveFields(t *testing.T) {
	t.Parallel()
	var response IngestResponse
	err := decodeJSON(strings.NewReader(`{"config_version":2,"future_field":"allowed"}`), &response)
	if err != nil {
		t.Fatal(err)
	}
	if response.ConfigVersion != 2 {
		t.Fatalf("config version = %d", response.ConfigVersion)
	}
}

func TestControllerURLsRequireHTTPSEvenForLoopback(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]"} {
		if _, err := validateHTTPSURL(raw); err == nil {
			t.Errorf("validateHTTPSURL(%q) accepted plaintext HTTP", raw)
		}
	}
}

func TestIngestURLMustRemainOnEnrolledControllerOrigin(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		controller string
		ingest     string
		wantError  bool
	}{
		{"https://controller.example", "https://controller.example/v1/ingest", false},
		{"https://controller.example", "https://controller.example:443/v1/ingest", false},
		{"https://controller.example:8443", "https://controller.example:8443/v1/ingest", false},
		{"https://controller.example", "https://internal.example/v1/ingest", true},
		{"https://controller.example", "https://controller.example:8443/v1/ingest", true},
	} {
		err := ValidateIngestURL(test.controller, test.ingest)
		if (err != nil) != test.wantError {
			t.Errorf("ValidateIngestURL(%q, %q) = %v", test.controller, test.ingest, err)
		}
	}
}
