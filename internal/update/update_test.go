package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSelectsPlatformKindAndStagesVerifiedArtifact(t *testing.T) {
	t.Parallel()
	body := []byte("signed pulse artifact")
	digest := sha256.Sum256(body)
	indexBody := fmt.Appendf(nil, `{"schema_version":1,"future_field":"allowed","artifacts":[{"version":"1.2.0","os":"linux","arch":"amd64","kind":"headless","filename":"pulse","url":"pulse","sha256":"%s","size_bytes":%d},{"version":"9.0.0","os":"windows","arch":"amd64","kind":"headless","url":"ignored","sha256":"%s"}]}`, hex.EncodeToString(digest[:]), len(body), hex.EncodeToString(digest[:]))
	server, publicKey := newSignedUpdateServer(t, indexBody, body)
	defer server.Close()
	manager := Manager{IndexURL: server.URL + "/main/index.json", OS: "linux", Arch: "amd64", publicKey: publicKey}
	available, err := manager.Check(context.Background(), "1.0.0", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if available.Version != "1.2.0" || available.URL != server.URL+"/main/pulse" {
		t.Fatalf("unexpected update: %+v", available)
	}
	staged, err := manager.Stage(context.Background(), available, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(staged)
	if err != nil || string(got) != string(body) {
		t.Fatalf("staged artifact = %q, %v", got, err)
	}
}

func TestActivateRollsBackBrokenBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds tiny health-check scripts")
	}
	directory := t.TempDir()
	active := filepath.Join(directory, "axon-pulse")
	staged := filepath.Join(directory, "staged")
	if err := os.WriteFile(active, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Activate(context.Background(), staged, active); err == nil {
		t.Fatal("Activate accepted a broken staged executable")
	}
	content, err := os.ReadFile(active)
	if err != nil || string(content) != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("active executable was not rolled back: %q, %v", content, err)
	}
}

func TestStageRejectsTamperingAndRemovesPartial(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("tampered")) }))
	defer server.Close()
	manager := Manager{}
	artifact := Artifact{Version: "1.1.0", URL: server.URL + "/pulse", SHA256: strings.Repeat("0", 64), SizeBytes: int64(len("tampered"))}
	available := Available{Artifact: artifact, metadataVerified: true, verifiedArtifact: artifact}
	if _, err := manager.Stage(context.Background(), available, t.TempDir()); err == nil {
		t.Fatal("Stage accepted a tampered artifact")
	}
}

func TestCheckRejectsInsecureDistributionURL(t *testing.T) {
	t.Parallel()
	_, err := (Manager{IndexURL: "http://dist.example/index.json"}).Check(context.Background(), "1.0.0", "headless")
	if err == nil {
		t.Fatal("Check accepted a non-HTTPS distribution URL")
	}
}

func TestCheckIgnoresRawDesktopArtifactAtSameVersion(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("0", 64)
	indexBody := fmt.Appendf(nil, `{"schema_version":1,"artifacts":[{"version":"1.2.0","os":"windows","arch":"amd64","kind":"desktop-raw","filename":"axon-pulse-desktop_windows_amd64.exe","url":"raw.exe","sha256":"%s","size_bytes":1},{"version":"1.2.0","os":"windows","arch":"amd64","kind":"desktop","filename":"axon-pulse_windows_amd64_setup.exe","url":"setup.exe","sha256":"%s","size_bytes":1}]}`, digest, digest)
	server, publicKey := newSignedUpdateServer(t, indexBody, nil)
	defer server.Close()
	available, err := (Manager{IndexURL: server.URL + "/main/index.json", OS: "windows", Arch: "amd64", publicKey: publicKey}).Check(context.Background(), "1.1.0", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if available.Filename != "axon-pulse_windows_amd64_setup.exe" {
		t.Fatalf("selected unsafe desktop artifact: %+v", available)
	}
}

func TestCheckRequiresExplicitArtifactKind(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("0", 64)
	indexBody := fmt.Appendf(nil, `{"schema_version":1,"artifacts":[{"version":"1.2.0","os":"linux","arch":"amd64","url":"legacy","sha256":"%s"}]}`, digest)
	server, publicKey := newSignedUpdateServer(t, indexBody, nil)
	defer server.Close()
	_, err := (Manager{IndexURL: server.URL + "/index.json", OS: "linux", Arch: "amd64", publicKey: publicKey}).Check(context.Background(), "1.1.0", "headless")
	if !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("Check accepted an unclassified artifact: %v", err)
	}
}

func TestCheckRejectsTamperedIndexSignature(t *testing.T) {
	t.Parallel()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, ".sig") {
			_, _ = fmt.Fprintln(writer, base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
			return
		}
		_, _ = writer.Write([]byte(`{"schema_version":1,"artifacts":[]}`))
	}))
	defer server.Close()
	_, err = (Manager{IndexURL: server.URL + "/index.json", publicKey: publicKey}).Check(context.Background(), "1.0.0", "headless")
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("Check signature error = %v", err)
	}
}

func TestStageRequiresAuthenticatedMetadata(t *testing.T) {
	t.Parallel()
	available := Available{Artifact: Artifact{Version: "1.1.0", URL: "https://dist.example/pulse", SHA256: strings.Repeat("0", 64)}}
	if _, err := (Manager{}).Stage(context.Background(), available, t.TempDir()); err == nil {
		t.Fatal("Stage accepted unauthenticated metadata")
	}
}

func TestCheckRejectsUnsupportedIndexSchema(t *testing.T) {
	t.Parallel()
	server, publicKey := newSignedUpdateServer(t, []byte(`{"schema_version":2,"artifacts":[]}`), nil)
	defer server.Close()
	_, err := (Manager{IndexURL: server.URL + "/index.json", publicKey: publicKey}).Check(context.Background(), "1.0.0", "headless")
	if err == nil || !strings.Contains(err.Error(), "unsupported update index schema 2") {
		t.Fatalf("Check schema error = %v", err)
	}
}

func TestCheckRejectsArtifactWithoutPositiveSize(t *testing.T) {
	t.Parallel()
	digest := strings.Repeat("0", 64)
	indexBody := fmt.Appendf(nil, `{"schema_version":1,"artifacts":[{"version":"1.2.0","os":"linux","arch":"amd64","kind":"headless","url":"pulse","sha256":"%s"}]}`, digest)
	server, publicKey := newSignedUpdateServer(t, indexBody, nil)
	defer server.Close()
	_, err := (Manager{IndexURL: server.URL + "/index.json", OS: "linux", Arch: "amd64", publicKey: publicKey}).Check(context.Background(), "1.0.0", "headless")
	if err == nil || !strings.Contains(err.Error(), "between 1 byte and 100 MiB") {
		t.Fatalf("Check size error = %v", err)
	}
}

func FuzzReleaseIndexArtifactValidation(f *testing.F) {
	validDigest := strings.Repeat("0", sha256.Size*2)
	f.Add(fmt.Appendf(nil, `{"schema_version":1,"artifacts":[{"version":"1.2.0","url":"https://dist.example/pulse","sha256":"%s","size_bytes":1}]}`, validDigest))
	f.Add(fmt.Appendf(nil, `{"schema_version":1,"releases":[{"version":"1.2.0","artifacts":[{"url":"https://dist.example/pulse","sha256":"%s"}]}]}`, validDigest))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if int64(len(data)) > maxIndexBytes {
			t.Skip()
		}
		var document index
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&document); err != nil {
			return
		}
		artifacts := append([]Artifact(nil), document.Artifacts...)
		for _, item := range document.Releases {
			for _, artifact := range item.Artifacts {
				if artifact.Version == "" {
					artifact.Version = item.Version
				}
				artifacts = append(artifacts, artifact)
			}
		}
		for _, artifact := range artifacts {
			if artifact.Version == "" {
				artifact.Version = document.Version
			}
			if validateArtifact(artifact) == nil && (artifact.SizeBytes <= 0 || artifact.SizeBytes > maxArtifactBytes) {
				t.Fatalf("accepted out-of-bounds artifact size %d", artifact.SizeBytes)
			}
		}
	})
}

func newSignedUpdateServer(t *testing.T, indexBody, artifactBody []byte) (*httptest.Server, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(privateKey, indexBody)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "index.json.sig"):
			_, _ = fmt.Fprintln(writer, base64.StdEncoding.EncodeToString(signature))
		case strings.HasSuffix(request.URL.Path, "index.json"):
			_, _ = writer.Write(indexBody)
		default:
			_, _ = writer.Write(artifactBody)
		}
	}))
	return server, publicKey
}

func TestProgressWriterReportsInBoundedStepsAndFlushesTheTail(t *testing.T) {
	t.Parallel()
	var reports [][2]int64
	writer := &progressWriter{total: 1_000_000, report: func(downloaded, total int64) { reports = append(reports, [2]int64{downloaded, total}) }}
	chunk := make([]byte, 100<<10)
	for range 7 {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	// 700 KiB written: reports at the 256 KiB and 512 KiB boundaries only.
	if len(reports) != 2 || reports[0][0] < progressStep || reports[1][0] < 2*progressStep || reports[1][1] != 1_000_000 {
		t.Fatalf("reports = %v", reports)
	}
	writer.flush()
	if len(reports) != 3 || reports[2][0] != int64(7*len(chunk)) {
		t.Fatalf("tail flush missing: %v", reports)
	}
	writer.flush()
	if len(reports) != 3 {
		t.Fatal("flush without new bytes reported again")
	}
}
