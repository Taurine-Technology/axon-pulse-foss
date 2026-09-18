package service

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// replaceFile mimics dpkg: write the new file beside the old one and rename it
// over the top, then age it past the settle window.
func replaceFile(t *testing.T, path string, contents string) {
	t.Helper()
	staged := path + ".dpkg-new"
	if err := os.WriteFile(staged, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func packageWatchedService(t *testing.T) (*Service, string) {
	t.Helper()
	daemon, err := Open(t.TempDir(), shortSocketPath(t, "pulse-pkg-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	path := filepath.Join(t.TempDir(), "axon-pulse")
	replaceFile(t, path, "#!/bin/sh\nexit 0\n")
	daemon.updateMethod = "apt"
	daemon.updates = nil
	daemon.packagePath = path
	info, err := statInstalledPackage(path)
	if err != nil {
		t.Fatal(err)
	}
	daemon.packageInfo = info
	daemon.restartDelay = 0
	return daemon, path
}

func TestAPTBuildRestartsInPlaceWhenThePackageIsReplaced(t *testing.T) {
	daemon, path := packageWatchedService(t)
	now := time.Now()
	// Unchanged file: nothing happens, and the check is rate-limited.
	daemon.checkInstalledPackage(context.Background(), now)
	daemon.wg.Wait()
	if _, restart := daemon.PackageRestart(); restart || daemon.updateBusy {
		t.Fatal("unchanged package triggered a restart")
	}
	replaceFile(t, path, "#!/bin/sh\nexit 1\n")
	daemon.checkInstalledPackage(context.Background(), now.Add(30*time.Second))
	daemon.wg.Wait()
	if _, restart := daemon.PackageRestart(); restart {
		t.Fatal("package check ran before the minute interval elapsed")
	}
	daemon.checkInstalledPackage(context.Background(), now.Add(2*packageCheckInterval))
	daemon.wg.Wait()
	got, restart := daemon.PackageRestart()
	if !restart || got != path {
		t.Fatalf("PackageRestart = %q, %v", got, restart)
	}
	select {
	case <-daemon.shutdown:
	default:
		t.Fatal("run loop was not asked to stop")
	}
	if !daemon.updateStatus.Restarting || !daemon.updateBusy {
		t.Fatal("restart must hold the update-busy state so no speed test starts")
	}
}

func TestAPTBuildDefersPackageRestartWhileBusy(t *testing.T) {
	daemon, path := packageWatchedService(t)
	replaceFile(t, path, "#!/bin/sh\nexit 1\n")
	daemon.mu.Lock()
	daemon.speedBusy = true
	daemon.mu.Unlock()
	daemon.checkInstalledPackage(context.Background(), time.Now().Add(2*packageCheckInterval))
	daemon.wg.Wait()
	if _, restart := daemon.PackageRestart(); restart || daemon.updateBusy {
		t.Fatal("restart started during a speed test")
	}
	daemon.mu.Lock()
	daemon.speedBusy = false
	daemon.mu.Unlock()
	daemon.checkInstalledPackage(context.Background(), time.Now().Add(4*packageCheckInterval))
	daemon.wg.Wait()
	if _, restart := daemon.PackageRestart(); !restart {
		t.Fatal("restart did not resume once the service was idle")
	}
}

func TestAPTBuildIgnoresAFreshlyReplacedPackageUntilItSettles(t *testing.T) {
	daemon, path := packageWatchedService(t)
	replaceFile(t, path, "#!/bin/sh\nexit 1\n")
	// The file was replaced "just now" relative to the clock the check sees.
	now := time.Now().Add(2 * packageCheckInterval)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	daemon.checkInstalledPackage(context.Background(), now)
	daemon.wg.Wait()
	if _, restart := daemon.PackageRestart(); restart {
		t.Fatal("restarted while the package unpack may still be in progress")
	}
}

func TestSelfUpdatingBuildDoesNotWatchThePackage(t *testing.T) {
	daemon, err := Open(t.TempDir(), shortSocketPath(t, "pulse-self-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	if daemon.updateMethod == "apt" || daemon.packageInfo != nil {
		t.Fatalf("portable build is watching %q", daemon.packagePath)
	}
}
