// Package update implements channel-aware, checksum-verified Pulse updates.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

type (
	Artifact struct {
		Version   string `json:"version"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
		Kind      string `json:"kind,omitempty"`
		Filename  string `json:"filename,omitempty"`
		URL       string `json:"url"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes,omitempty"`
	}

	release struct {
		Version   string     `json:"version"`
		Artifacts []Artifact `json:"artifacts"`
	}

	index struct {
		SchemaVersion int        `json:"schema_version,omitempty"`
		GeneratedAt   string     `json:"generated_at,omitempty"`
		Version       string     `json:"version,omitempty"`
		Artifacts     []Artifact `json:"artifacts,omitempty"`
		Releases      []release  `json:"releases,omitempty"`
	}

	Available struct {
		Artifact

		IndexURL         string `json:"index_url"`
		metadataVerified bool
		verifiedArtifact Artifact
	}

	Manager struct {
		IndexURL string
		Client   *http.Client
		OS       string
		Arch     string
		// Progress, when set, receives the bytes staged so far and the
		// artifact's expected size while Stage downloads, so a UI can show
		// real progress instead of an indeterminate wait. It is called from
		// the download goroutine and must return quickly.
		Progress  func(downloaded, total int64)
		publicKey ed25519.PublicKey
	}

	// progressWriter reports cumulative bytes to a Manager.Progress callback
	// at most every progressStep bytes, so a large download does not turn into
	// a flood of notifications.
	progressWriter struct {
		total    int64
		written  int64
		reported int64
		report   func(downloaded, total int64)
	}
)

const (
	// DesktopInstanceID is the desktop app's single-instance identifier. The
	// macOS update helper locates the running GUI through the lock file Wails
	// keeps under this name in the user's temporary directory.
	DesktopInstanceID = "com.taurine.axon-pulse"
	maxArtifactBytes  = int64(100 << 20)
	// progressStep is the byte interval between download progress reports.
	progressStep         = 256 << 10
	maxIndexBytes        = int64(2 << 20)
	maxSignatureBytes    = int64(4 << 10)
	supportedIndexSchema = 1
)

var (
	// embeddedIndexPublicKey is populated in release builds with -X. Keeping the
	// default empty makes development builds fail closed instead of trusting an
	// unauthenticated update index.
	embeddedIndexPublicKey string

	ErrNoUpdate = errors.New("already up to date")
)

func (m Manager) Check(ctx context.Context, currentVersion, kind string) (Available, error) {
	if InstallMethod() == "apt" {
		return Available{}, ErrPackageManaged
	}
	if err := secureURL(m.IndexURL); err != nil {
		return Available{}, fmt.Errorf("update index: %w", err)
	}
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.IndexURL, nil)
	if err != nil {
		return Available{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Available{}, fmt.Errorf("fetch update index: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Available{}, fmt.Errorf("fetch update index: HTTP %d", response.StatusCode)
	}
	if response.Request != nil {
		if err := secureURL(response.Request.URL.String()); err != nil {
			return Available{}, fmt.Errorf("update index redirect: %w", err)
		}
	}
	indexBytes, err := readLimited(response.Body, maxIndexBytes)
	if err != nil {
		return Available{}, fmt.Errorf("read update index: %w", err)
	}
	if err := m.verifyIndexSignature(ctx, client, indexBytes); err != nil {
		return Available{}, err
	}
	var document index
	decoder := json.NewDecoder(bytes.NewReader(indexBytes))
	if err := decoder.Decode(&document); err != nil {
		return Available{}, fmt.Errorf("decode update index: %w", err)
	}
	if document.SchemaVersion > supportedIndexSchema {
		return Available{}, fmt.Errorf("unsupported update index schema %d", document.SchemaVersion)
	}
	artifacts := append([]Artifact(nil), document.Artifacts...)
	for i := range artifacts {
		if artifacts[i].Version == "" {
			artifacts[i].Version = document.Version
		}
	}
	for _, item := range document.Releases {
		for _, artifact := range item.Artifacts {
			if artifact.Version == "" {
				artifact.Version = item.Version
			}
			artifacts = append(artifacts, artifact)
		}
	}
	wantedOS, wantedArch := m.OS, m.Arch
	if wantedOS == "" {
		wantedOS = runtime.GOOS
	}
	if wantedArch == "" {
		wantedArch = runtime.GOARCH
	}
	current := canonicalVersion(currentVersion)
	var selected Artifact
	for _, artifact := range artifacts {
		if artifact.OS != wantedOS || artifact.Arch != wantedArch || (kind != "" && artifact.Kind != kind) {
			continue
		}
		version := canonicalVersion(artifact.Version)
		if !semver.IsValid(version) || (semver.IsValid(current) && semver.Compare(version, current) <= 0) {
			continue
		}
		if selected.Version == "" || semver.Compare(version, canonicalVersion(selected.Version)) > 0 {
			selected = artifact
		}
	}
	if selected.Version == "" {
		return Available{}, ErrNoUpdate
	}
	resolved, err := resolveArtifactURL(m.IndexURL, selected.URL)
	if err != nil {
		return Available{}, err
	}
	selected.URL = resolved
	if err := validateArtifact(selected); err != nil {
		return Available{}, err
	}
	return Available{Artifact: selected, IndexURL: m.IndexURL, metadataVerified: true, verifiedArtifact: selected}, nil
}

func (m Manager) Stage(ctx context.Context, available Available, stateDir string) (string, error) {
	if InstallMethod() == "apt" {
		return "", ErrPackageManaged
	}
	if !available.metadataVerified || available.Artifact != available.verifiedArtifact {
		return "", errors.New("update metadata was not authenticated")
	}
	if err := validateArtifact(available.Artifact); err != nil {
		return "", err
	}
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, available.URL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download update: HTTP %d", response.StatusCode)
	}
	if response.Request != nil {
		if err := secureURL(response.Request.URL.String()); err != nil {
			return "", fmt.Errorf("update download redirect: %w", err)
		}
	}
	directory := filepath.Join(stateDir, "updates", strings.TrimPrefix(canonicalVersion(available.Version), "v"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create update directory: %w", err)
	}
	filename := filepath.Base(available.Filename)
	if filename == "." || filename == "" {
		parsed, _ := url.Parse(available.URL)
		filename = filepath.Base(parsed.Path)
	}
	if filename == "." || filename == "" || filename == string(filepath.Separator) {
		filename = "axon-pulse.update"
	}
	temporary := filepath.Join(directory, filename+".part")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create staged update: %w", err)
	}
	hash := sha256.New()
	sink := io.MultiWriter(file, hash)
	if m.Progress != nil {
		progress := &progressWriter{total: available.SizeBytes, report: m.Progress}
		sink = io.MultiWriter(sink, progress)
		defer progress.flush()
	}
	written, copyErr := io.Copy(sink, io.LimitReader(response.Body, maxArtifactBytes+1))
	syncErr, closeErr := file.Sync(), file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("write staged update: %w", errors.Join(copyErr, syncErr, closeErr))
	}
	if written > maxArtifactBytes || written != available.SizeBytes {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("staged update size %d does not match expected %d", written, available.SizeBytes)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != available.SHA256 {
		_ = os.Remove(temporary)
		return "", errors.New("staged update checksum does not match release index")
	}
	staged := strings.TrimSuffix(temporary, ".part")
	if err := os.Rename(temporary, staged); err != nil {
		return "", fmt.Errorf("finalize staged update: %w", err)
	}
	return staged, nil
}

func (w *progressWriter) Write(chunk []byte) (int, error) {
	w.written += int64(len(chunk))
	if w.written-w.reported >= progressStep {
		w.flush()
	}
	return len(chunk), nil
}

func (w *progressWriter) flush() {
	if w.report == nil || w.written == w.reported {
		return
	}
	w.reported = w.written
	w.report(w.written, w.total)
}

func (m Manager) verifyIndexSignature(ctx context.Context, client *http.Client, indexBytes []byte) error {
	publicKey, err := m.verificationKey()
	if err != nil {
		return err
	}
	signatureURL, err := detachedSignatureURL(m.IndexURL)
	if err != nil {
		return fmt.Errorf("update signature URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signatureURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch update index signature: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch update index signature: HTTP %d", response.StatusCode)
	}
	if response.Request != nil {
		if err := secureURL(response.Request.URL.String()); err != nil {
			return fmt.Errorf("update signature redirect: %w", err)
		}
	}
	encoded, err := readLimited(response.Body, maxSignatureBytes)
	if err != nil {
		return fmt.Errorf("read update index signature: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("update index signature is invalid")
	}
	if !ed25519.Verify(publicKey, indexBytes, signature) {
		return errors.New("update index signature verification failed")
	}
	return nil
}

func (m Manager) verificationKey() (ed25519.PublicKey, error) {
	if len(m.publicKey) == ed25519.PublicKeySize {
		return append(ed25519.PublicKey(nil), m.publicKey...), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(embeddedIndexPublicKey))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(embeddedIndexPublicKey))
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("update verification key is not configured")
	}
	return ed25519.PublicKey(decoded), nil
}

func detachedSignatureURL(indexURL string) (string, error) {
	parsed, err := url.Parse(indexURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("URL is invalid")
	}
	parsed.Path += ".sig"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}

func canonicalVersion(version string) string {
	version = strings.TrimSpace(version)
	if version != "" && !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return version
}

func validateArtifact(artifact Artifact) error {
	if !semver.IsValid(canonicalVersion(artifact.Version)) {
		return fmt.Errorf("invalid update version %q", artifact.Version)
	}
	if len(artifact.SHA256) != sha256.Size*2 {
		return errors.New("update SHA-256 must contain 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return errors.New("update SHA-256 must contain 64 lowercase hexadecimal characters")
	}
	if artifact.SizeBytes <= 0 || artifact.SizeBytes > maxArtifactBytes {
		return errors.New("update size must be between 1 byte and 100 MiB")
	}
	return secureURL(artifact.URL)
}

func resolveArtifactURL(indexURL, raw string) (string, error) {
	base, err := url.Parse(indexURL)
	if err != nil {
		return "", err
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(reference).String()
	if err := secureURL(resolved); err != nil {
		return "", fmt.Errorf("artifact URL: %w", err)
	}
	return resolved, nil
}

func secureURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("URL is invalid")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "127.0.0.1" || host == "::1" || host == "localhost") {
		return nil
	}
	return errors.New("URL must use HTTPS")
}
