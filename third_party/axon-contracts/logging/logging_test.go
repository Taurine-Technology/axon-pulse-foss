package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Taurine-Technology/axon-contracts/gen/go/logging"
)

// parseAttrs is a tiny helper that scans a slog text-format line for
// key=value pairs and returns a map. It handles quoted values (e.g.
// msg="hello world") by stripping surrounding double-quotes.
func parseAttrs(line string) map[string]string {
	attrs := make(map[string]string)
	// slog TextHandler emits: time=... level=... msg=... key=val ...
	// Split on whitespace-separated tokens; each token is key=value.
	// Quoted values may contain spaces, but the keys we care about here
	// (time, level, component) don't, so simple splitting is fine.
	for tok := range strings.FieldsSeq(line) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		// Strip surrounding double-quotes if present.
		v = strings.Trim(v, `"`)
		attrs[k] = v
	}
	return attrs
}

// TestLevelFiltering verifies that the level set via AXON_LOG_LEVEL controls
// which records actually reach the output.
func TestLevelFiltering(t *testing.T) {
	t.Setenv("INVOCATION_ID", "") // not under systemd
	t.Setenv("AXON_LOG_LEVEL", "warn")

	var buf bytes.Buffer
	log := logging.SetupWriter(&buf, "test")

	log.Debug("debug-msg")
	log.Info("info-msg")
	log.Warn("warn-msg")
	log.Error("error-msg")

	out := buf.String()
	if strings.Contains(out, "debug-msg") {
		t.Errorf("debug message should have been filtered; got: %s", out)
	}
	if strings.Contains(out, "info-msg") {
		t.Errorf("info message should have been filtered; got: %s", out)
	}
	if !strings.Contains(out, "warn-msg") {
		t.Errorf("warn message should appear; got: %s", out)
	}
	if !strings.Contains(out, "error-msg") {
		t.Errorf("error message should appear; got: %s", out)
	}
}

// TestComponentAttr verifies that every log record carries component=<name>.
func TestComponentAttr(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "debug")

	var buf bytes.Buffer
	log := logging.SetupWriter(&buf, "mycomp")

	log.Info("hello")

	line := strings.TrimSpace(buf.String())
	attrs := parseAttrs(line)

	if got, want := attrs["component"], "mycomp"; got != want {
		t.Errorf("component attr: got %q, want %q; full line: %s", got, want, line)
	}
}

// TestNoTimestampUnderSystemd verifies that the time attr is absent when
// INVOCATION_ID is set (journald run).
func TestNoTimestampUnderSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "abc123") // simulate systemd
	t.Setenv("AXON_LOG_LEVEL", "debug")

	var buf bytes.Buffer
	log := logging.SetupWriter(&buf, "svc")

	log.Info("systemd-msg")

	line := strings.TrimSpace(buf.String())
	if strings.Contains(line, "time=") {
		t.Errorf("expected no time= attr under systemd; got: %s", line)
	}
}

// TestTimestampOutsideSystemd verifies that the time attr IS present when
// INVOCATION_ID is not set (regular terminal / non-systemd run).
func TestTimestampOutsideSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "debug")

	var buf bytes.Buffer
	log := logging.SetupWriter(&buf, "svc")

	log.Info("plain-msg")

	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, "time=") {
		t.Errorf("expected time= attr outside systemd; got: %s", line)
	}
}

// TestInvalidLevelFallsBackToInfo verifies that an unrecognised AXON_LOG_LEVEL
// value silently falls back to INFO (the warning is a side-effect we don't
// test for in automated checks, but debug messages must be absent).
func TestInvalidLevelFallsBackToInfo(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "nonsense")

	var buf bytes.Buffer
	log := logging.SetupWriter(&buf, "fallback")

	log.Debug("should-be-hidden")
	log.Info("should-appear")

	out := buf.String()
	if strings.Contains(out, "should-be-hidden") {
		t.Errorf("debug should be filtered on invalid level; got: %s", out)
	}
	if !strings.Contains(out, "should-appear") {
		t.Errorf("info should appear on invalid level; got: %s", out)
	}
}

// TestNewSubcomponent verifies that New() produces a child logger whose
// component attr is "parent/subcomponent".
func TestNewSubcomponent(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "debug")

	var buf bytes.Buffer
	parent := logging.SetupWriter(&buf, "parent")

	child := logging.New(parent, "child")
	buf.Reset()
	child.Info("sub-msg")

	line := strings.TrimSpace(buf.String())
	attrs := parseAttrs(line)

	if got, want := attrs["component"], "parent/child"; got != want {
		t.Errorf("child component: got %q, want %q; full line: %s", got, want, line)
	}
	// Regression guard for the duplicate-component bug: a New-derived logger
	// must emit the component key exactly once, never twice.
	if got := strings.Count(line, "component="); got != 1 {
		t.Errorf("component= count: got %d, want 1; full line: %s", got, line)
	}
}

// TestNewChainedNoDuplicateComponent verifies that chaining New multiple times
// composes the path-like component name AND still emits the component key
// exactly once.  Before the fix each New() re-wrapped an existing
// componentHandler, so an N-deep chain emitted component= N times.
func TestNewChainedNoDuplicateComponent(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "debug")

	var buf bytes.Buffer
	a := logging.SetupWriter(&buf, "a")
	c := logging.New(logging.New(a, "b"), "c")

	buf.Reset()
	c.Info("chained-msg")

	line := strings.TrimSpace(buf.String())
	attrs := parseAttrs(line)

	if got, want := attrs["component"], "a/b/c"; got != want {
		t.Errorf("chained component: got %q, want %q; full line: %s", got, want, line)
	}
	if got := strings.Count(line, "component="); got != 1 {
		t.Errorf("component= count on 3-deep chain: got %d, want 1; full line: %s", got, line)
	}
}

// TestSetupUsesStdout is a smoke-test that Setup() returns a non-nil logger.
// The actual writer destination (os.Stdout) cannot be easily captured in unit
// tests, so we only check that the call succeeds and the returned logger works.
func TestSetupUsesStdout(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("AXON_LOG_LEVEL", "info")

	log := logging.Setup("smoke")
	if log == nil {
		t.Fatal("Setup returned nil")
	}
	// Ensure Enabled works (sanity-check, does not panic).
	_ = log.Enabled(context.TODO(), slog.LevelInfo)
}
