package support

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRotatingLoggerRedactsAnyValuesBeforePlatformSink(t *testing.T) {
	t.Parallel()
	var platform bytes.Buffer
	base := slog.New(slog.NewTextHandler(&platform, nil))
	logger, closer, err := NewRotatingLogger(t.TempDir(), base)
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("upload failed", "error", errors.New("sensor_secret=hunter2 at https://example.test/private?token=spt_bad"), "payload", map[string]any{"token": "also-secret"})
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	text := platform.String()
	for _, forbidden := range []string{"hunter2", "spt_bad", "example.test", "also-secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("platform log contains %q: %s", forbidden, text)
		}
	}
}

func TestRotatingWriterBoundsAndRedactsLogs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pulse.log")
	writer, err := openRotatingWriter(path, 256, 2)
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := writer.Write([]byte("sensor_secret=hunter2 endpoint=https://example.test/probe?token=spt_bad " + strings.Repeat("x", 80))); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("rotation file %s: %v", candidate, err)
		}
		if info.Size() > 256 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
			// Windows has no unix permission bits; per-user ACLs protect the file.
			t.Fatalf("rotation file %s size=%d mode=%o", candidate, info.Size(), info.Mode().Perm())
		}
		data, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"hunter2", "spt_bad", "example.test"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("log %s contains %q: %s", candidate, forbidden, data)
			}
		}
	}
}
