package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shortSocketPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows IPC listens on named pipes, which live in the pipe
		// namespace rather than the filesystem.
		return `\\.\pipe\pulse-ipc-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	}
	// /tmp keeps the socket path under the 104-byte sun_path limit that
	// t.TempDir can exceed on macOS.
	dir, err := os.MkdirTemp("/tmp", "pulse-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "pulse.sock")
}

func TestWatchReceivesPushMessages(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	path := shortSocketPath(t)
	updates := make(chan any, 1)
	server := &Server{Path: path, Handler: HandlerFunc(func(_ context.Context, _ Request) (any, *Error) {
		return Stream{C: updates}, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ctx) }()
	received := make(chan string, 1)
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go func() {
		_ = Watch(watchCtx, path, "watch", nil, func(raw json.RawMessage) {
			var value map[string]string
			_ = json.Unmarshal(raw, &value)
			received <- value["state"]
		})
	}()
	updates <- map[string]string{"state": "active"}
	if got := <-received; got != "active" {
		t.Fatalf("state = %q", got)
	}
	stopWatch()
	cancel()
}

func TestServerRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	path := shortSocketPath(t)
	server := &Server{Path: path, Handler: HandlerFunc(func(_ context.Context, request Request) (any, *Error) {
		if request.Method != "status" {
			return nil, &Error{Code: "unknown", Message: "unknown method"}
		}
		return map[string]string{"state": "active"}, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	var result map[string]string
	if err := Call(context.Background(), path, "status", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result["state"] != "active" {
		t.Fatalf("result = %+v", result)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnknownIPCMethodReturnsTypedError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := shortSocketPath(t)
	server := &Server{Path: path, Handler: HandlerFunc(func(_ context.Context, _ Request) (any, *Error) {
		return nil, &Error{Code: "unknown", Message: "unknown method"}
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ctx) }()
	err := Call(context.Background(), path, "wat", nil, nil)
	if err == nil || err.Error() != "unknown method" {
		t.Fatalf("error = %v", err)
	}
}

func TestWatchStreamsLeaveCapacityForUnaryCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := shortSocketPath(t)
	server := &Server{Path: path, Handler: HandlerFunc(func(_ context.Context, request Request) (any, *Error) {
		if request.Method == "watch" {
			updates := make(chan any, 1)
			updates <- map[string]string{"state": "active"}
			return Stream{C: updates}, nil
		}
		return map[string]string{"state": "active"}, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx) }()

	watchCtx, stopWatches := context.WithCancel(context.Background())
	defer stopWatches()
	for range maxConcurrentStreams {
		established := make(chan struct{}, 1)
		go func() {
			_ = Watch(watchCtx, path, "watch", nil, func(json.RawMessage) {
				established <- struct{}{}
			})
		}()
		select {
		case <-established:
		case <-time.After(2 * time.Second):
			t.Fatal("watch stream did not become active")
		}
	}

	extraCtx, stopExtra := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopExtra()
	if err := Watch(extraCtx, path, "watch", nil, nil); err == nil || !strings.Contains(err.Error(), "too many active") {
		t.Fatalf("extra Watch error = %v", err)
	}
	var status map[string]string
	if err := Call(context.Background(), path, "status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if status["state"] != "active" {
		t.Fatalf("status = %+v", status)
	}

	stopWatches()
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestServerCloseTerminatesIdleConnectionAndServe(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	server := &Server{Path: path, Handler: HandlerFunc(func(context.Context, Request) (any, *Error) {
		return nil, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	connection, err := dialLocal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.mu.Lock()
		accepted := len(server.connections) == 1
		server.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not accept idle connection")
		}
		time.Sleep(time.Millisecond)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close did not terminate an idle accepted connection")
	}
}

func TestServeBoundsNonCooperativeHandlerDrain(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := &Server{
		Path: path, DrainTimeout: 20 * time.Millisecond,
		Handler: HandlerFunc(func(context.Context, Request) (any, *Error) {
			close(started)
			<-release
			return nil, nil
		}),
	}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()
	callDone := make(chan error, 1)
	go func() { callDone <- Call(context.Background(), path, "block", nil, nil) }()
	<-started
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, ErrDrainTimeout) {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not enforce handler drain timeout")
	}
	close(release)
	select {
	case <-server.Drained():
	case <-time.After(time.Second):
		t.Fatal("drained signal did not close after handler returned")
	}
	<-callDone
}

func TestServerCloseBeforeListenDoesNotRemoveUnownedPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows named pipes are not filesystem paths")
	}
	path := filepath.Join(t.TempDir(), "not-owned")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{Path: path, Handler: HandlerFunc(func(context.Context, Request) (any, *Error) { return nil, nil })}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "keep" {
		t.Fatalf("unowned IPC path after Close = %q, %v", contents, err)
	}
	if err := server.Listen(); err == nil {
		t.Fatal("closed server started listening again")
	}
}

func TestServerCloseTerminatesActiveWatch(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	updates := make(chan any, 1)
	updates <- map[string]string{"state": "active"}
	server := &Server{Path: path, Handler: HandlerFunc(func(context.Context, Request) (any, *Error) {
		return Stream{C: updates}, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()
	watchDone := make(chan error, 1)
	established := make(chan struct{}, 1)
	go func() {
		watchDone <- Watch(context.Background(), path, "watch", nil, func(json.RawMessage) {
			established <- struct{}{}
		})
	}()
	select {
	case <-established:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not become active")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close did not terminate active watch")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not drain after active watch closed")
	}
}
