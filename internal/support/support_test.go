package support

import (
	"errors"
	"strings"
	"testing"
)

func TestEncodeRedactsAdversarialIdentifiers(t *testing.T) {
	t.Parallel()
	input := map[string]any{
		"sensor_secret":         "do-not-ship",
		"error":                 "Bearer abc.def api_key=hunter2 at https://example.test/probe?token=spt_bad from 192.0.2.1 fe80::1%en0 00:11:22:33:44:55 /Users/alice/private/file",
		"nested":                []any{map[string]any{"ssid": "Alice WiFi", "hostname": "alice-mac"}},
		"token=dynamic-map-key": 1,
		"2001:db8::1":           "dynamic-ip-key",
	}
	encoded, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"do-not-ship", "abc.def", "hunter2", "spt_bad", "192.0.2.1", "fe80::1", "2001:db8::1", "dynamic-map-key", "00:11:22:33:44:55", "Alice WiFi", "alice-mac", "/Users/alice"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("bundle contains %q: %s", forbidden, text)
		}
	}
}

func TestEncodeRejectsOversizedBundle(t *testing.T) {
	t.Parallel()
	values := make([]string, MaxBundleBytes/100)
	for index := range values {
		values[index] = strings.Repeat("a", 100)
	}
	_, err := Encode(map[string]any{"counts": values})
	var sizeErr *SizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("error = %v, want *SizeError", err)
	}
}

func TestEncodeKeepsNonSensitiveFields(t *testing.T) {
	t.Parallel()
	encoded, err := Encode(map[string]any{"recent_record_counts": map[string]any{"minute_metric": 42}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "minute_metric") || !strings.Contains(string(encoded), "42") {
		t.Fatalf("redaction removed diagnostic counts: %s", encoded)
	}
}

func TestEncodeIsDeterministicForCollidingRedactedKeys(t *testing.T) {
	t.Parallel()
	first, err := Encode(map[string]any{"192.0.2.1": "one", "198.51.100.2": "two"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(map[string]any{"198.51.100.2": "two", "192.0.2.1": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("bundle encoding drifted:\n%s\n%s", first, second)
	}
}

func TestCompileHostnamePatternRedactsDeviceHostname(t *testing.T) {
	t.Parallel()
	pattern := compileHostnamePattern("Subscriber-MacBook.local", nil)
	if pattern == nil {
		t.Fatal("expected a pattern for a normal hostname")
	}
	for input, want := range map[string]string{
		"dial subscriber-macbook.local:443 failed": "dial [hostname]:443 failed",
		"host Subscriber-MacBook offline":          "host [hostname] offline",
		"unrelated macbook text":                   "unrelated macbook text",
	} {
		if got := pattern.ReplaceAllString(input, "[hostname]"); got != want {
			t.Fatalf("redact(%q) = %q, want %q", input, got, want)
		}
	}
	short := compileHostnamePattern("amy", nil)
	if short == nil {
		t.Fatal("short personal hostnames must still be redacted")
	}
	if got := short.ReplaceAllString("host amy.local slow", "[hostname]"); got != "host [hostname] slow" {
		t.Fatalf("short redact = %q", got)
	}
	if compileHostnamePattern("a", nil) != nil {
		t.Fatal("single-character hostnames must not produce a pattern")
	}
	if compileHostnamePattern("", errors.New("hostname unavailable")) != nil {
		t.Fatal("hostname errors must not produce a pattern")
	}
}
