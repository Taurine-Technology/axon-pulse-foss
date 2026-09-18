package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAPTBuildRejectsChecksAndDownloadsBeforeNetwork(t *testing.T) {
	previous := installMethod
	installMethod = "apt"
	t.Cleanup(func() { installMethod = previous })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	manager := Manager{IndexURL: server.URL + "/index.json"}
	if got := InstallMethod(); got != "apt" {
		t.Fatalf("install method = %q", got)
	}
	if _, err := manager.Check(context.Background(), "1.0.0", "headless"); !errors.Is(err, ErrPackageManaged) {
		t.Fatalf("check error = %v", err)
	}
	// Even a stale manual stage request must fail before metadata validation,
	// network access or filesystem writes.
	if _, err := manager.Stage(context.Background(), Available{}, t.TempDir()); !errors.Is(err, ErrPackageManaged) {
		t.Fatalf("stage error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("APT build made %d update requests", got)
	}
}

func TestPortableBuildKeepsSelfUpdates(t *testing.T) {
	previous := installMethod
	installMethod = ""
	t.Cleanup(func() { installMethod = previous })
	if got := InstallMethod(); got != "self" {
		t.Fatalf("install method = %q", got)
	}
}
