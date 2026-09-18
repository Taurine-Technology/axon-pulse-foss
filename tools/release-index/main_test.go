package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactKindKeepsRawDesktopOutOfUpdateChannel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"axon-pulse-desktop_windows_amd64.exe": "desktop-raw",
		"axon-pulse-viewer_linux_arm64":        "desktop-raw",
		"axon-pulse_windows_amd64_setup.exe":   "desktop",
		"axon-pulse_macos_universal.dmg":       "desktop",
		"axon-pulse_linux_amd64.AppImage":      "desktop",
		"axon-pulse_linux_amd64":               "headless",
		"axon-pulse_linux_amd64.tar.gz":        "headless-package",
		"axon-pulse_windows_amd64_managed.zip": "managed-package",
	}
	for filename, want := range cases {
		if got := artifactKind(filename); got != want {
			t.Errorf("artifactKind(%q) = %q, want %q", filename, got, want)
		}
	}
}

func TestValidSemver(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1.2.3", "v1.2.3", "1.2.3-rc.1", "1.2.3+build.5"} {
		if !validSemver(version) {
			t.Errorf("validSemver(%q) = false", version)
		}
	}
	for _, version := range []string{"", "1.2", "01.2.3", "1.2.3-01", "release-1.2.3"} {
		if validSemver(version) {
			t.Errorf("validSemver(%q) = true", version)
		}
	}
}

func TestLoadSigningKeyAcceptsSeed(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "update-signing-key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(privateKey.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !privateKey.Equal(loaded) {
		t.Fatal("loaded signing key differs")
	}
}

func TestParseVerificationKey(t *testing.T) {
	t.Parallel()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := parseVerificationKey(base64.RawStdEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	if !publicKey.Equal(loaded) {
		t.Fatal("loaded verification key differs")
	}
}
