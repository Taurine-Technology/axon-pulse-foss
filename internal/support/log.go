package support

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type (
	teeHandler struct {
		left  slog.Handler
		right slog.Handler
	}

	rotatingWriter struct {
		mu      sync.Mutex
		path    string
		maximum int64
		backups int
		file    *os.File
		size    int64
	}
)

const (
	MaxLogBytes = int64(512 << 10)
	LogBackups  = 2
)

// NewRotatingLogger keeps a small, user-private local log alongside the
// platform logger. Both sinks receive the same centrally redacted record.
func NewRotatingLogger(stateDir string, base *slog.Logger) (*slog.Logger, io.Closer, error) {
	if base == nil {
		base = slog.Default()
	}
	directory := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, nil, err
	}
	writer, err := openRotatingWriter(filepath.Join(directory, "pulse.log"), MaxLogBytes, LogBackups)
	if err != nil {
		return nil, nil, err
	}
	local := slog.NewJSONHandler(writer, nil)
	return slog.New(&teeHandler{left: base.Handler(), right: local}), writer, nil
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.left.Enabled(ctx, level) || h.right.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	record = redactRecord(record)
	return errorsJoin(h.left.Handle(ctx, record), h.right.Handle(ctx, record))
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	attrs = redactAttrs(attrs)
	return &teeHandler{left: h.left.WithAttrs(attrs), right: h.right.WithAttrs(attrs)}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{left: h.left.WithGroup(RedactString(name)), right: h.right.WithGroup(RedactString(name))}
}

func redactRecord(record slog.Record) slog.Record {
	redacted := slog.NewRecord(record.Time, record.Level, RedactString(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		redacted.AddAttrs(redactAttr(attr))
		return true
	})
	return redacted
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(attrs))
	for index, attr := range attrs {
		out[index] = redactAttr(attr)
	}
	return out
}

func redactAttr(attr slog.Attr) slog.Attr {
	attr.Key = RedactString(attr.Key)
	if sensitiveKey(attr.Key) {
		return slog.String("[redacted-key]", "[redacted]")
	}
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindString {
		attr.Value = slog.StringValue(RedactString(attr.Value.String()))
	}
	if attr.Value.Kind() == slog.KindGroup {
		attr.Value = slog.GroupValue(redactAttrs(attr.Value.Group())...)
	}
	// Errors and arbitrary structured values arrive as KindAny. Convert them
	// to a bounded redacted representation before either the local or platform
	// sink sees the record; the local writer's second pass is only defence in
	// depth and must not be the first privacy boundary.
	if attr.Value.Kind() == slog.KindAny {
		attr.Value = slog.StringValue(RedactString(fmt.Sprint(attr.Value.Any())))
	}
	return attr
}

func errorsJoin(left, right error) error {
	if left != nil {
		return left
	}
	return right
}

func openRotatingWriter(path string, maximum int64, backups int) (*rotatingWriter, error) {
	writer := &rotatingWriter{path: path, maximum: maximum, backups: backups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file, w.size = file, info.Size()
	return nil
}

func (w *rotatingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLength := len(data)
	// redactCore keeps the serialized JSON record intact; per-attribute values
	// were already bounded by RedactString before encoding. A record that still
	// exceeds the file bound is replaced with a valid substitute rather than
	// truncated into broken JSON.
	line := []byte(redactCore(strings.TrimSpace(string(data))) + "\n")
	if int64(len(line)) > w.maximum {
		line = []byte(`{"level":"WARN","msg":"[log record dropped: exceeded size bound]"}` + "\n")
	}
	if w.size+int64(len(line)) > w.maximum {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(line)
	w.size += int64(written)
	if err != nil {
		return 0, err
	}
	return originalLength, nil
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file, w.size = nil, 0
	for index := w.backups; index >= 1; index-- {
		target := w.path + "." + strconv.Itoa(index)
		if index == w.backups {
			_ = os.Remove(target)
		}
		source := w.path
		if index > 1 {
			source = w.path + "." + strconv.Itoa(index-1)
		}
		if err := os.Rename(source, target); err != nil && !os.IsNotExist(err) {
			// Reopen the live file so a failed rename (e.g. another process
			// holding the file open on Windows) surfaces as one failed write
			// instead of poisoning every later write with a closed handle.
			if openErr := w.open(); openErr != nil {
				return errors.Join(err, openErr)
			}
			return err
		}
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
