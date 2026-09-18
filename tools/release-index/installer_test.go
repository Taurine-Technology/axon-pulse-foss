package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedInstallerRejectsTamperedIndex(t *testing.T) {
	t.Parallel()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	index := document{SchemaVersion: 1, Artifacts: []artifact{
		{Version: "0.0.3", OS: "linux", Arch: "amd64", Kind: "headless", Filename: "axon-pulse_linux_amd64", SHA256: strings.Repeat("a", 64), SizeBytes: 100},
		{Version: "0.0.3", OS: "linux", Arch: "arm64", Kind: "headless", Filename: "axon-pulse_linux_arm64", SHA256: strings.Repeat("b", 64), SizeBytes: 200},
	}}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, data))
	if err := os.WriteFile(path+".sig", []byte(signature), 0o600); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(publicKey)
	if _, err := installerFromPublishedIndex(path, "../../packaging/install.sh.in", strings.Repeat("A", 40), key); err != nil {
		t.Fatal(err)
	}
	if _, err := installerFromPublishedIndex(path, "../../packaging/install.sh.in", "", key); err == nil {
		t.Fatal("accepted bootstrap generation without an APT signing fingerprint")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installerFromPublishedIndex(path, "../../packaging/install.sh.in", strings.Repeat("A", 40), key); err == nil {
		t.Fatal("accepted an index whose exact bytes no longer match its signature")
	}
}

func TestRenderInstaller(t *testing.T) {
	t.Parallel()
	fingerprint := strings.Repeat("A", 40)
	result, err := renderInstaller("../../packaging/install.sh.in", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := os.ReadFile("../../packaging/apt/install-apt.sh.in")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(strings.TrimSpace(string(shared)), `main "$@"`)
	body = strings.ReplaceAll(body, "@FINGERPRINT@", fingerprint)
	if !strings.Contains(string(result), body) {
		t.Fatal("bootstrap must contain the complete shared APT installer")
	}
	if !strings.HasSuffix(string(result), "main --package headless \"$@\"\n") {
		t.Fatal("bootstrap must select headless and forward all invocation arguments")
	}
	for _, obsolete := range []string{"@APT_INSTALLER@", "@FINGERPRINT@", "@VERSION@", "@AMD64_SHA256@", "@SERVICE_UNIT@", "axon-pulse.installing"} {
		if strings.Contains(string(result), obsolete) {
			t.Errorf("bootstrap retained obsolete data %q", obsolete)
		}
	}
	if strings.Count(string(result), "\nmain ") != 1 {
		t.Fatal("embedded main must only be invoked by the headless wrapper")
	}
	for _, fingerprint := range []string{"", strings.Repeat("a", 40), strings.Repeat("A", 39), strings.Repeat("G", 40), "'; touch /tmp/unsafe; #"} {
		if _, err := renderInstaller("../../packaging/install.sh.in", fingerprint); err == nil {
			t.Errorf("accepted invalid fingerprint %q", fingerprint)
		}
	}
	if _, err := renderInstaller("../../packaging/install.sh.in", strings.Repeat("B", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestRenderInstallerRejectsBrokenTemplates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, wrapper, shared string }{
		{"missing wrapper placeholder", "no placeholder", ""},
		{"duplicate wrapper placeholder", "@APT_INSTALLER@ @APT_INSTALLER@", ""},
		{"missing fingerprint", "@APT_INSTALLER@", "main() { :; }\nmain \"$@\"\n"},
		{"missing invocation", "@APT_INSTALLER@", "main() { fingerprint='@FINGERPRINT@'; }\n"},
		{"extra commands", "@APT_INSTALLER@", "main() { fingerprint='@FINGERPRINT@'; }\nmain \"$@\"\necho unexpected\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "apt"), 0o700); err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(root, "install.sh.in")
			if err := os.WriteFile(wrapper, []byte(test.wrapper), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "apt", "install-apt.sh.in"), []byte(test.shared), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := renderInstaller(wrapper, strings.Repeat("A", 40)); err == nil {
				t.Fatal("accepted an incomplete or unexpectedly structured template")
			}
		})
	}
}
