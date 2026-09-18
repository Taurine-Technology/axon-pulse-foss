// Package logging provides structured logging helpers built on top of the
// standard library's log/slog package.
//
// # Journald compatibility
//
// When the INVOCATION_ID environment variable is set (which systemd injects for
// every unit it starts) the logger omits the time attribute from each record;
// journald already stamps every line with a precise timestamp, so including one
// in the message text creates redundant noise and confuses log-indexers.
//
// Outside systemd the time attribute is included so that plain terminal output
// remains self-contained.
//
// # Level selection
//
// The log level is read from the AXON_LOG_LEVEL environment variable.
// Accepted values (case-insensitive): debug, info, warn, error.
// The default is info. An unrecognised value falls back to info and emits a
// single warning line to stderr so the misconfiguration is visible without
// breaking the agent.
//
// # Component hierarchy
//
// Every logger carries a "component" attribute that names the subsystem
// producing the record.  Use New to derive a child logger; the child's
// component becomes "<parent-component>/<subcomponent>", which is both
// human-readable and easily grep-able:
//
//	grep 'component=control-plane/mqtt' agent.log
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// componentKey is the slog attribute key used for the subsystem name.
const (
	componentKey = "component"
)

// Setup creates a new [*slog.Logger] that writes text-formatted records to
// os.Stdout.  It is the primary entry-point for production use; tests should
// call SetupWriter instead.
//
// Both AXON_LOG_LEVEL and INVOCATION_ID are read exactly once, here at Setup
// time, and baked into the returned logger.  Changing those environment
// variables afterwards has no effect on an already-constructed logger; call
// Setup again to pick up new values.
func Setup(component string) *slog.Logger {
	return setup(os.Stdout, component)
}

// SetupWriter is the injectable variant of Setup used by tests (and any future
// caller that needs to redirect output).  The writer w is used directly; no
// buffering or closing is performed.
func SetupWriter(w io.Writer, component string) *slog.Logger {
	return setup(w, component)
}

// setup is the internal constructor shared by Setup and SetupWriter.
func setup(w io.Writer, component string) *slog.Logger {
	level := parseLevel()

	// When running under systemd, INVOCATION_ID is injected by the service
	// manager.  Journald adds its own timestamp, so we omit the slog "time"
	// attr to avoid duplication.
	underSystemd := os.Getenv("INVOCATION_ID") != ""

	var replaceAttr func(groups []string, a slog.Attr) slog.Attr
	if underSystemd {
		replaceAttr = func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{} // drop
			}
			return a
		}
	}

	h := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})

	// Wrap the text handler in a componentHandler so every record carries a
	// single component attr.  New derives children by unwrapping this handler
	// (see baseHandler) and re-wrapping the underlying text handler exactly
	// once, so the component key is never duplicated however deeply loggers
	// are chained.
	return slog.New(&componentHandler{inner: h, component: component})
}

// New derives a child logger from parent.  The child's component attribute
// is set to "<parent-component>/<subcomponent>" so log lines remain
// grep-friendly (e.g. grep 'component=control-plane/mqtt' agent.log).
//
// Convention: child component = "<parent-component>/<subcomponent>".
// This is intentionally a path-like separator so the component value is both
// human-readable and easily grep-able with a simple string match.
func New(parent *slog.Logger, subcomponent string) *slog.Logger {
	parentComponent := extractComponent(parent)
	var childComponent string
	if parentComponent == "" {
		childComponent = subcomponent
	} else {
		childComponent = parentComponent + "/" + subcomponent
	}
	// Wrap the parent's *base* handler (the non-componentHandler underneath any
	// existing component wrappers) in a single fresh componentHandler.  If we
	// instead wrapped parent.Handler() directly, a parent that is itself a
	// componentHandler would stay in the chain and emit its own component attr,
	// producing component= twice (and N times for N-deep chains).  Unwrapping
	// guarantees exactly one componentHandler, so every record carries the
	// composed name once.
	return slog.New(&componentHandler{inner: baseHandler(parent.Handler()), component: childComponent})
}

// componentHandler is a slog.Handler wrapper that injects a fixed
// "component=<name>" attribute into every record it handles.
//
// It does not deduplicate component attrs on its own: if its inner handler is
// also a componentHandler, both will emit the key.  Single-component output is
// instead guaranteed by construction — Setup wraps a plain text handler once,
// and New unwraps to that base handler (see baseHandler) before re-wrapping —
// so exactly one componentHandler is ever present in a handler chain.
type (
	componentHandler struct {
		inner     slog.Handler
		component string
	}
)

func (h *componentHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *componentHandler) Handle(ctx context.Context, r slog.Record) error {
	// Prepend the component attr so it appears near the front of each line.
	// slog.Record.AddAttrs appends; we build a new record to prepend instead.
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	nr.AddAttrs(slog.String(componentKey, h.component))
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *componentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &componentHandler{
		inner:     h.inner.WithAttrs(attrs),
		component: h.component,
	}
}

func (h *componentHandler) WithGroup(name string) slog.Handler {
	return &componentHandler{
		inner:     h.inner.WithGroup(name),
		component: h.component,
	}
}

// extractComponent returns the component name stored in a componentHandler, or
// "" if the logger uses a different handler type.
func extractComponent(l *slog.Logger) string {
	if ch, ok := l.Handler().(*componentHandler); ok {
		return ch.component
	}
	return ""
}

// baseHandler unwraps any componentHandler layers and returns the first handler
// underneath that is not a componentHandler.  This lets New build a child from
// the original text handler instead of re-wrapping an existing componentHandler,
// which would cause the component attr to be emitted more than once.  A handler
// that is not (or no longer wraps) a componentHandler is returned unchanged.
func baseHandler(h slog.Handler) slog.Handler {
	for {
		ch, ok := h.(*componentHandler)
		if !ok {
			return h
		}
		h = ch.inner
	}
}

// parseLevel reads AXON_LOG_LEVEL and returns the corresponding slog.Level.
// If the variable is empty, info is returned.
// If the variable contains an unrecognised value, a warning is printed to
// stderr and info is returned.
// "warning" is accepted as an alias for "warn" for operator convenience.
func parseLevel() slog.Level {
	raw := strings.TrimSpace(os.Getenv("AXON_LOG_LEVEL"))
	if raw == "" {
		return slog.LevelInfo
	}
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		fmt.Fprintf(os.Stderr,
			"logging: unrecognised AXON_LOG_LEVEL=%q; falling back to info\n", raw)
		return slog.LevelInfo
	}
}
