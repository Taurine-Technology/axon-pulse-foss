package service

import (
	"context"
	"os"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/state"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
)

// APT-managed builds cannot replace their own executable, but they can notice
// that the package manager did. Once a minute the service stats the path it
// was started from; when that path names a different inode (dpkg unpacks the
// new file and renames it over the old one) the service drains like it does
// for a self-update, flushes, and asks its entry point to re-exec the
// installed binary in place. The PID survives, so systemd (headless) and the
// detached desktop child both carry on without a logout or a maintainer
// script touching user sessions. One stat per minute is the whole cost.
const (
	packageCheckInterval = time.Minute
	// A replaced file must be this old before it is trusted: dpkg's rename is
	// atomic, but a multi-file unpack may still be in progress.
	packageSettle = 10 * time.Second
)

// watchInstalledPackage records the inode of the installed executable so a
// later replacement is detectable. It is a no-op for self-updating builds.
func (s *Service) watchInstalledPackage() {
	if s.updateMethod != "apt" {
		return
	}
	path, err := pulseupdate.InstalledExecutable()
	if err != nil {
		s.logger.Warn("package_watch_unavailable", "error", err)
		return
	}
	info, err := statInstalledPackage(path)
	if err != nil {
		s.logger.Warn("package_watch_unavailable", "error", err)
		return
	}
	s.packagePath, s.packageInfo = path, info
}

// statInstalledPackage captures the executable's identity through an open
// handle. os.Stat loads Windows file identity lazily from the path on the
// first comparison, so a FileInfo taken by path would describe whichever file
// the path names later, not the one that was installed when the watch began.
func statInstalledPackage(path string) (os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return file.Stat()
}

// checkInstalledPackage runs from tick: cheap, once a minute, and only while
// no restart is already underway.
func (s *Service) checkInstalledPackage(ctx context.Context, now time.Time) {
	if s.packageInfo == nil || now.Sub(s.packageCheckedAt) < packageCheckInterval {
		return
	}
	s.packageCheckedAt = now
	info, err := os.Stat(s.packagePath)
	if err != nil || os.SameFile(s.packageInfo, info) || now.Sub(info.ModTime()) < packageSettle {
		return
	}
	s.mu.Lock()
	busy := s.updateBusy || s.speedBusy || s.uploadBusy || s.updateStatus.Restarting
	if !busy {
		// Hold updateBusy so a speed test cannot start while we drain.
		s.updateBusy = true
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if busy {
		return
	}
	go func() {
		defer s.wg.Done()
		if !s.restartForPackageUpgrade(ctx) {
			s.clearBusy("update")
		}
	}()
}

// restartForPackageUpgrade mirrors the tail of stage_update: quiesce admitted
// work, flush telemetry, then close the run loop with a restart request.
// Returns false when the restart was deferred (the caller retries next minute).
func (s *Service) restartForPackageUpgrade(ctx context.Context) bool {
	s.updateRun.Lock()
	defer s.updateRun.Unlock()
	drainCtx, cancelDrain := context.WithTimeout(ctx, s.updateDrainWait)
	resumeActivity, err := s.activity.quiesce(drainCtx)
	cancelDrain()
	if err != nil {
		s.logger.Info("package_restart_deferred", "error", err)
		return false
	}
	s.modeRun.Lock()
	flushErr := s.flushForUpdate(ctx, time.Now(), s.store.Snapshot().Mode == state.ModeStandalone)
	s.modeRun.Unlock()
	if flushErr != nil {
		resumeActivity()
		s.logger.Warn("package_restart_flush_failed", "error", flushErr)
		return false
	}
	s.mu.Lock()
	s.updateStatus.Restarting = true
	s.packageRestart = true
	s.mu.Unlock()
	s.logger.Info("package_upgrade_restart", "executable", s.packagePath, "version", serviceVersion())
	s.wg.Go(func() {
		timer := time.NewTimer(s.restartDelay)
		defer timer.Stop()
		<-timer.C
		s.shutdownOnce.Do(func() { close(s.shutdown) })
	})
	return true
}

// PackageRestart reports whether Run stopped because the installed package was
// replaced, and the executable the entry point should re-exec.
func (s *Service) PackageRestart() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packagePath, s.packageRestart
}
