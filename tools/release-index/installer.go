package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Bootstrap an already-published channel without rebuilding or changing its
// artifacts/metadata. A trusted repository public key authenticates the index.
func installerFromPublishedIndex(indexPath, templatePath, fingerprint, key string) ([]byte, error) {
	publicKey, err := parseVerificationKey(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}
	signatureText, err := os.ReadFile(indexPath + ".sig")
	if err != nil {
		return nil, err
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureText)))
	if err != nil || !ed25519.Verify(publicKey, data, signature) {
		return nil, fmt.Errorf("published index signature is invalid")
	}
	var index document
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	if index.SchemaVersion != 1 || len(index.Artifacts) == 0 {
		return nil, fmt.Errorf("unsupported or empty published index")
	}
	return renderInstaller(templatePath, fingerprint)
}

// renderInstaller embeds the shared APT bootstrap with its archive signing-key
// fingerprint. HTTPS delivery is the initial trust root; APT authenticates all
// subsequent metadata and packages. No raw executable or remote script runs.
func renderInstaller(templatePath, fingerprint string) ([]byte, error) {
	if !regexp.MustCompile(`^(?:[A-F0-9]{40}|[A-F0-9]{64})$`).MatchString(fingerprint) {
		return nil, fmt.Errorf("--apt-signing-fingerprint must be a full uppercase primary key fingerprint")
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, err
	}
	if strings.Count(string(template), "@APT_INSTALLER@") != 1 {
		return nil, fmt.Errorf("installer template must contain @APT_INSTALLER@ exactly once")
	}
	shared, err := os.ReadFile(filepath.Join(filepath.Dir(templatePath), "apt", "install-apt.sh.in"))
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(shared))
	const invocation = `main "$@"`
	if !strings.HasSuffix(body, "\n"+invocation) || strings.Count(body, "@FINGERPRINT@") != 1 {
		return nil, fmt.Errorf("shared APT installer must end with main invocation and contain exactly one fingerprint placeholder")
	}
	body = strings.TrimSuffix(body, invocation)
	body = strings.ReplaceAll(body, "@FINGERPRINT@", fingerprint)
	return []byte(strings.ReplaceAll(string(template), "@APT_INSTALLER@", body)), nil
}
