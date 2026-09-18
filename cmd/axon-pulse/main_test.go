package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
)

func TestConnectTokenSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		var err error
		dir, err = os.MkdirTemp("/tmp", "pulse-connect-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	socket := filepath.Join(dir, "pulse.sock")
	if runtime.GOOS == "windows" {
		socket = `\\.\pipe\pulse-connect-` + filepath.Base(dir)
	}
	received := make(chan map[string]string, 1)
	server := &ipc.Server{Path: socket, Handler: ipc.HandlerFunc(func(_ context.Context, request ipc.Request) (any, *ipc.Error) {
		var params map[string]string
		if request.Method != "connect" || json.Unmarshal(request.Params, &params) != nil {
			return nil, &ipc.Error{Code: "invalid", Message: "unexpected request"}
		}
		received <- params
		return map[string]string{"controller_url": params["url"], "sensor_id": "test-sensor", "state": "active"}, nil
	})}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	const token = "spt_test_private_token"
	for _, args := range [][]string{{"--token-stdin"}, {"--token", token}} {
		var stdout, stderr bytes.Buffer
		args = append(args, "--url", "https://controller.example", "--socket", socket)
		if code := connect(args, strings.NewReader(token+"\n"), &stdout, &stderr); code != 0 {
			t.Fatalf("connect failed with code %d: %s", code, stderr.String())
		}
		got := <-received
		if got["token"] != token || got["url"] != "https://controller.example" {
			t.Fatal("connect changed enrollment parameters")
		}
		if strings.Contains(stdout.String()+stderr.String(), token) {
			t.Fatal("connect exposed the token in output")
		}
	}
}

func TestConnectRejectsInvalidTokenInput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--url", "https://controller.example", "--token-stdin", "--token", "spt_secret"},
		{"--url", "https://controller.example", "--token-stdin"},
		{"--token-stdin"},
	} {
		var stdout, stderr bytes.Buffer
		if code := connect(args, strings.NewReader(""), &stdout, &stderr); code != 2 {
			t.Fatalf("connect returned %d instead of a usage error", code)
		}
		if strings.Contains(stdout.String()+stderr.String(), "spt_secret") {
			t.Fatal("usage error exposed a token")
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "version",
			args:       []string{"version"},
			wantCode:   0,
			wantStdout: "axon 0.0.0-dev",
		},
		{
			name:       "help",
			args:       []string{"help"},
			wantCode:   0,
			wantStdout: "Usage:",
		},
		{
			name:       "missing command",
			args:       []string{},
			wantCode:   2,
			wantStderr: "Usage:",
		},
		{
			name:       "unknown command",
			args:       []string{"wat"},
			wantCode:   2,
			wantStderr: "unknown command \"wat\"",
		},
		{
			name:       "unknown update stream",
			args:       []string{"channel", "nightly"},
			wantCode:   2,
			wantStderr: "channel accepts main, beta, or alpha",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			gotCode := run(test.args, &stdout, &stderr)
			if gotCode != test.wantCode {
				t.Fatalf("run code = %d, want %d", gotCode, test.wantCode)
			}
			if !strings.Contains(stdout.String(), test.wantStdout) {
				t.Fatalf("stdout = %q, want substring %q", stdout.String(), test.wantStdout)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestWritePrivateFileRestrictsExistingTarget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "support.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		// Windows has no unix permission bits; per-user ACLs protect the file.
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Fatalf("contents = %q, error = %v", data, err)
	}
}
