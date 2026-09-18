// Package support builds bounded, privacy-safe diagnostic bundles.
package support

import (
	"encoding/json"
	"maps"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type (
	SizeError struct{ Bytes int }
)

const (
	MaxBundleBytes = 256 << 10
)

var (
	tokenPattern          = regexp.MustCompile(`(?i)\b(?:spt_[A-Za-z0-9._-]+|bearer\s+[A-Za-z0-9._~+/=-]+)\b`)
	credentialPairPattern = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|sensor[_-]?secret|secret|password|token)\s*[:=]\s*[^\s&;,]+`)
	ipv4Pattern           = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Pattern           = regexp.MustCompile(`(?i)\[?(?:[0-9a-f]{0,4}:){2,}[0-9a-f]{0,4}(?:%[0-9a-z_.-]+)?\]?`)
	macPattern            = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)
	urlPattern            = regexp.MustCompile(`(?i)https?://[^\s"']+`)
	userPathPattern       = regexp.MustCompile(`(?i)(?:/Users/|/home/|[A-Z]:\\Users\\)[^\s"']+`)
	identityPairPattern   = regexp.MustCompile(`(?i)\b(?:ssid|bssid|hostname|network[_-]?identity)\s*[:=]\s*[^\s&;,\]}"']+`)
	hostnamePattern       = compileHostnamePattern(os.Hostname())
)

func WritePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// compileHostnamePattern targets this device's own hostname (with or without a
// domain suffix), which frequently embeds a personal name. Word boundaries keep
// the match to the standalone name; a single character is not identifying and
// would redact ordinary words, so it is skipped.
func compileHostnamePattern(hostname string, err error) *regexp.Regexp {
	if err != nil {
		return nil
	}
	short, _, _ := strings.Cut(strings.TrimSpace(hostname), ".")
	if len(short) < 2 {
		return nil
	}
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(short) + `(?:\.[A-Za-z0-9-]+)*\b`)
}

// RedactString is the single redaction path for logs, errors, and future
// consented support uploads. Bundles avoid identifiers by construction; this
// second layer protects against adversarial error strings.
func RedactString(value string) string {
	return truncateUTF8(redactCore(value), 512)
}

// redactCore applies every redaction rule without bounding the result, for
// callers such as the log writer that must keep a serialized record intact.
func redactCore(value string) string {
	value = tokenPattern.ReplaceAllString(value, "[credential]")
	value = credentialPairPattern.ReplaceAllString(value, "[credential]")
	value = identityPairPattern.ReplaceAllString(value, "[identity]")
	value = urlPattern.ReplaceAllStringFunc(value, func(raw string) string {
		if strings.Contains(raw, "?") {
			return "[url-with-query]"
		}
		return "[url]"
	})
	value = macPattern.ReplaceAllString(value, "[mac]")
	if hostnamePattern != nil {
		value = hostnamePattern.ReplaceAllString(value, "[hostname]")
	}
	value = ipv4Pattern.ReplaceAllString(value, "[ip]")
	value = ipv6Pattern.ReplaceAllStringFunc(value, redactIPv6)
	value = userPathPattern.ReplaceAllString(value, "[user-path]")
	return value
}

// truncateUTF8 bounds value to limit bytes without splitting a multi-byte rune.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

// Encode recursively redacts every string and enforces the support-bundle
// size ceiling. The caller should pass summaries, never raw payloads.
func Encode(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	redactValue(decoded)
	encoded, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return nil, err
	}
	// The cap applies to the final written file, including the trailing newline.
	if len(encoded)+1 > MaxBundleBytes {
		return nil, &SizeError{Bytes: len(encoded) + 1}
	}
	return append(encoded, '\n'), nil
}

func (e *SizeError) Error() string { return "support bundle exceeds the 256 KiB local limit" }

func redactValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		redacted := make(map[string]any, len(typed))
		for _, key := range keys {
			child := typed[key]
			redactedKey := RedactString(key)
			if sensitiveKey(key) {
				redactedKey = "[redacted-key]"
			}
			if redactedKey == "[redacted-key]" {
				child = "[redacted]"
			} else if text, ok := child.(string); ok {
				child = RedactString(text)
			} else {
				redactValue(child)
			}
			redacted[uniqueKey(redacted, redactedKey)] = child
		}
		clear(typed)
		maps.Copy(typed, redacted)
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = RedactString(text)
			} else {
				redactValue(child)
			}
		}
	}
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	return strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "password") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") ||
		lower == "ssid" || lower == "bssid" || lower == "hostname"
}

func uniqueKey(values map[string]any, key string) string {
	if _, exists := values[key]; !exists {
		return key
	}
	for index := 2; ; index++ {
		candidate := key + "-" + strconv.Itoa(index)
		if _, exists := values[candidate]; !exists {
			return candidate
		}
	}
}

func redactIPv6(raw string) string {
	value := strings.Trim(raw, "[]")
	if address, err := netip.ParseAddr(value); err == nil && address.Is6() {
		return "[ip]"
	}
	return raw
}
