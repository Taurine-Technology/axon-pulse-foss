// Command release-index creates an axon-dist-compatible channel manifest.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type (
	artifact struct {
		Version   string `json:"version"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
		Kind      string `json:"kind"`
		Filename  string `json:"filename"`
		URL       string `json:"url"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	}

	document struct {
		SchemaVersion int        `json:"schema_version"`
		GeneratedAt   time.Time  `json:"generated_at"`
		Artifacts     []artifact `json:"artifacts"`
	}
)

var (
	platformPattern = regexp.MustCompile(`_(linux|darwin|windows|macos)_(amd64|arm64|universal)(?:[_.]|$)`)
	semverPattern   = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

func main() {
	directory := flag.String("dir", "dist", "directory containing release artifacts")
	version := flag.String("version", "", "release version")
	signingKeyPath := flag.String("signing-key", "", "path to a base64-encoded Ed25519 private key or seed")
	verificationKey := flag.String("public-key", "", "base64-encoded Ed25519 public key embedded in release binaries")
	installerTemplate := flag.String("installer-template", "", "optional headless bootstrap template")
	aptFingerprint := flag.String("apt-signing-fingerprint", "", "full uppercase primary archive key fingerprint required for the bootstrap installer")
	existingIndex := flag.String("existing-index", "", "generate only install.sh from an existing signed index and its .sig file")
	flag.Parse()
	if *existingIndex != "" {
		if *installerTemplate == "" || *verificationKey == "" || *aptFingerprint == "" || *signingKeyPath != "" || *version != "" {
			fatal(fmt.Errorf("--existing-index requires --installer-template, --public-key and --apt-signing-fingerprint, without --version or --signing-key"))
		}
		installer, err := installerFromPublishedIndex(*existingIndex, *installerTemplate, *aptFingerprint, *verificationKey)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*directory, "install.sh"), installer, 0o644); err != nil {
			fatal(err)
		}
		return
	}
	if !validSemver(*version) {
		fmt.Fprintln(os.Stderr, "release-index: --version must be valid semantic versioning")
		os.Exit(2)
	}
	if *signingKeyPath == "" {
		fmt.Fprintln(os.Stderr, "release-index: --signing-key is required")
		os.Exit(2)
	}
	if *verificationKey == "" {
		fmt.Fprintln(os.Stderr, "release-index: --public-key is required")
		os.Exit(2)
	}
	signingKey, err := loadSigningKey(*signingKeyPath)
	if err != nil {
		fatal(err)
	}
	publicKey, err := parseVerificationKey(*verificationKey)
	if err != nil {
		fatal(err)
	}
	derived, ok := signingKey.Public().(ed25519.PublicKey)
	if !ok || !derived.Equal(publicKey) {
		fatal(fmt.Errorf("update signing key does not match the embedded public key"))
	}
	entries, err := os.ReadDir(*directory)
	if err != nil {
		fatal(err)
	}
	var artifacts []artifact
	checksums := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "index.json" || entry.Name() == "index.json.sig" || entry.Name() == "latest" || entry.Name() == "checksums.txt" {
			continue
		}
		platform := platformPattern.FindStringSubmatch(entry.Name())
		if len(platform) == 0 {
			continue
		}
		osName := platform[1]
		if osName == "macos" {
			osName = "darwin"
		}
		kind := artifactKind(entry.Name())
		digest, size, err := hashFile(filepath.Join(*directory, entry.Name()))
		if err != nil {
			fatal(err)
		}
		arches := []string{platform[2]}
		if platform[2] == "universal" {
			arches = []string{"amd64", "arm64"}
		}
		for _, arch := range arches {
			artifacts = append(artifacts, artifact{Version: *version, OS: osName, Arch: arch, Kind: kind, Filename: entry.Name(), URL: entry.Name(), SHA256: digest, SizeBytes: size})
		}
		checksums = append(checksums, digest+"  "+entry.Name())
	}
	if len(artifacts) == 0 {
		fatal(fmt.Errorf("no platform artifacts found in %s", *directory))
	}
	sort.Slice(artifacts, func(i, j int) bool {
		left, right := artifacts[i], artifacts[j]
		return left.OS+left.Arch+left.Kind+left.Filename < right.OS+right.Arch+right.Kind+right.Filename
	})
	if *installerTemplate != "" {
		installer, err := renderInstaller(*installerTemplate, *aptFingerprint)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*directory, "install.sh"), installer, 0o644); err != nil {
			fatal(err)
		}
		digest := sha256.Sum256(installer)
		checksums = append(checksums, hex.EncodeToString(digest[:])+"  install.sh")
		artifacts = append(artifacts, artifact{
			Version: *version, OS: "linux", Arch: "all", Kind: "bootstrap",
			Filename: "install.sh", URL: "install.sh", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(installer)),
		})
	}
	sort.Strings(checksums)
	data, err := json.MarshalIndent(document{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Artifacts: artifacts}, "", "  ")
	if err != nil {
		fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(*directory, "index.json"), data, 0o644); err != nil {
		fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(signingKey, data)) + "\n"
	if err := os.WriteFile(filepath.Join(*directory, "index.json.sig"), []byte(signature), 0o644); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*directory, "latest"), []byte(*version+"\n"), 0o644); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*directory, "checksums.txt"), []byte(strings.Join(checksums, "\n")+"\n"), 0o644); err != nil {
		fatal(err)
	}
}

func validSemver(value string) bool {
	matches := semverPattern.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil {
		return false
	}
	if matches[4] == "" {
		return true
	}
	for identifier := range strings.SplitSeq(matches[4], ".") {
		if len(identifier) > 1 && identifier[0] == '0' && allDigits(identifier) {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func loadSigningKey(path string) (ed25519.PrivateKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read update signing key: %w", err)
	}
	encoded := strings.TrimSpace(string(contents))
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("decode update signing key: %w", err)
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	default:
		return nil, fmt.Errorf("update signing key must contain a %d-byte seed or %d-byte private key", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func parseVerificationKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("update public key must contain %d base64-encoded bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

func artifactKind(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.Contains(lower, "setup") || strings.HasSuffix(lower, ".dmg") || strings.HasSuffix(lower, ".appimage"):
		return "desktop"
	case strings.Contains(lower, "managed"):
		return "managed-package"
	case strings.Contains(lower, "desktop") || strings.Contains(lower, "viewer"):
		// Raw GUI binaries are retained as build artifacts for packaging and
		// diagnosis, but update clients must select only a signed installer,
		// notarized DMG, or AppImage.
		return "desktop-raw"
	case strings.HasSuffix(lower, ".deb") || strings.HasSuffix(lower, ".tar.gz"):
		return "headless-package"
	default:
		return "headless"
	}
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release-index:", err)
	os.Exit(1)
}
