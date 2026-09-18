package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/support"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type (
	PulseService struct {
		socket    string
		mu        sync.Mutex
		claim     map[string]string
		claimGen  uint64
		autostart *application.AutostartManager
		dialogs   *application.DialogManager
		// relaunch hands the desktop over to a fresh instance of the
		// installed bundle; nil means the platform cannot do it in place.
		relaunch func() error
	}
)

// DesktopVersion is the version of this GUI build, as opposed to the
// service's version reported in Status. The two diverge when an update has
// restarted the service but this window is still the previous build.
func (p *PulseService) DesktopVersion() string {
	if buildinfo.Version == "" {
		return "0.0.0-dev"
	}
	return buildinfo.Version
}

// RelaunchDesktop quits this window and opens the installed bundle again so
// the interface matches the running service. The service keeps running.
func (p *PulseService) RelaunchDesktop() error {
	p.mu.Lock()
	relaunch := p.relaunch
	p.mu.Unlock()
	if relaunch == nil {
		return errors.New("relaunch is not supported on this platform; close and reopen Axon Pulse")
	}
	return relaunch()
}

func (p *PulseService) configureDialogs(manager *application.DialogManager) {
	p.mu.Lock()
	p.dialogs = manager
	p.mu.Unlock()
}

func (p *PulseService) configureAutostart(manager *application.AutostartManager) {
	p.mu.Lock()
	p.autostart = manager
	p.mu.Unlock()
}

func (p *PulseService) initializeAutostart(stateDir string) error {
	marker := filepath.Join(stateDir, "autostart-initialized")
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p.mu.Lock()
	manager := p.autostart
	p.mu.Unlock()
	if manager == nil {
		return errors.New("start at login is unavailable")
	}
	enabled, err := manager.IsEnabled()
	if err != nil {
		return err
	}
	if !enabled {
		if err := manager.Enable(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte("initialized\n"), 0o600)
}

func (p *PulseService) AutostartStatus() (bool, error) {
	p.mu.Lock()
	manager := p.autostart
	p.mu.Unlock()
	if manager == nil {
		return false, errors.New("start at login is unavailable")
	}
	return manager.IsEnabled()
}

func (p *PulseService) SetAutostart(enabled bool) error {
	p.mu.Lock()
	manager := p.autostart
	p.mu.Unlock()
	if manager == nil {
		return errors.New("start at login is unavailable")
	}
	if enabled {
		return manager.Enable()
	}
	return manager.Disable()
}

func NewPulseService(socket string) *PulseService {
	return &PulseService{socket: socket}
}

func (p *PulseService) Status() (any, error) {
	var result any
	err := p.call("status", map[string]bool{"live": true}, &result)
	return result, err
}

func (p *PulseService) History(hours int) (any, error) {
	var result any
	err := p.call("history", map[string]int{"hours": hours}, &result)
	return result, err
}

// SpeedTestHistory returns only speed-test records so long windows (the home
// screen scans 7 days) stay far below the IPC message size cap.
func (p *PulseService) SpeedTestHistory(hours int) (any, error) {
	var result any
	err := p.call("history", map[string]any{"hours": hours, "kinds": []string{"speed_test"}}, &result)
	return result, err
}

func (p *PulseService) SupportBundle() (any, error) {
	var result any
	err := p.call("support_bundle", nil, &result)
	return result, err
}

func (p *PulseService) ExportSupportBundle() (string, error) {
	var result any
	if err := p.call("support_bundle", nil, &result); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	dialogs := p.dialogs
	p.mu.Unlock()
	if dialogs == nil {
		return "", errors.New("save dialog is unavailable")
	}
	path, err := dialogs.SaveFile().SetFilename("axon-pulse-support-"+time.Now().Format("2006-01-02")+".json").AddFilter("JSON", "*.json").PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	if err := support.WritePrivateFile(filepath.Clean(path), append(data, '\n')); err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func (p *PulseService) Connect(controllerURL, token string) (any, error) {
	generation := p.claimGeneration()
	var result any
	err := p.call("connect", map[string]string{"url": controllerURL, "token": token}, &result)
	if err == nil {
		p.clearClaimAt(generation)
	}
	return result, err
}

func (p *PulseService) Pause(seconds int64) (any, error) {
	var result any
	err := p.call("pause", map[string]int64{"seconds": seconds}, &result)
	return result, err
}

func (p *PulseService) SetMode(mode string) (any, error) {
	generation := p.claimGeneration()
	var result any
	err := p.call("set_mode", map[string]string{"mode": mode}, &result)
	if err == nil && mode == "standalone" {
		p.clearClaimAt(generation)
	}
	return result, err
}

func (p *PulseService) SetSSIDConsent(enabled bool) (any, error) {
	var result any
	err := p.call("ssid_consent", map[string]bool{"enabled": enabled}, &result)
	return result, err
}

// RunTest starts a user-initiated test. The service runs it regardless of
// cross-traffic, battery, or metering gates (annotating the conditions in the
// result); only data budgets can still defer it.
func (p *PulseService) RunTest(profile string) (any, error) {
	var result any
	err := p.call("test", map[string]any{"profile": profile}, &result)
	return result, err
}

// SetHouseholdMix stores the local household activity mix override; a nil
// mix clears it so controller configuration or the default applies again.
// It returns the refreshed status so the UI can show the effective mix.
func (p *PulseService) SetHouseholdMix(mix map[string]any) (any, error) {
	var result any
	var params map[string]any
	if mix == nil {
		params = map[string]any{"mix": nil}
	} else {
		params = map[string]any{"mix": mix}
	}
	err := p.call("set_household_mix", params, &result)
	return result, err
}

func (p *PulseService) Disconnect() error {
	return p.call("disconnect", nil, nil)
}

func (p *PulseService) CheckUpdate() (any, error) {
	var result any
	err := p.call("check_update", nil, &result)
	return result, err
}

// SetUpdateChannel switches the release stream and returns the result of an
// immediate check against it.
func (p *PulseService) SetUpdateChannel(channel string) (any, error) {
	var result any
	err := p.call("set_update_channel", map[string]string{"channel": channel}, &result)
	return result, err
}

func (p *PulseService) StageUpdate() (any, error) {
	var result any
	err := p.call("stage_update", nil, &result)
	return result, err
}

// PendingClaim is a non-destructive peek: the claim survives until a Connect
// succeeds or the user chooses local mode, so a window that opens late (tray
// launch, macOS launch-by-URL) can still retrieve it.
func (p *PulseService) PendingClaim() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claim
}

// claimGeneration snapshots the claim counter before a slow call so a claim
// link installed while that call is in flight is not cleared by its success.
func (p *PulseService) claimGeneration() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claimGen
}

func (p *PulseService) clearClaimAt(generation uint64) {
	p.mu.Lock()
	if p.claimGen == generation {
		p.claim = nil
	}
	p.mu.Unlock()
}

func (p *PulseService) SetClaimURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "axon-pulse" || parsed.Host != "claim" {
		return errors.New("invalid Axon Pulse claim link")
	}
	controllerURL := strings.TrimSpace(parsed.Query().Get("url"))
	token := strings.TrimSpace(parsed.Query().Get("token"))
	if controllerURL == "" || !strings.HasPrefix(token, "spt_") {
		return errors.New("claim link is missing its controller or invite token")
	}
	p.mu.Lock()
	p.claim = map[string]string{"url": controllerURL, "token": token}
	p.claimGen++
	p.mu.Unlock()
	return nil
}

func (p *PulseService) Watch(ctx context.Context, receive func(any)) {
	// A service that re-execs after a package upgrade is back within a
	// couple of seconds; only a second consecutive failure is reported so
	// the tray and window do not flash "Service unavailable" for a blip.
	failures := 0
	for ctx.Err() == nil {
		err := ipc.Watch(ctx, p.socket, "watch", nil, func(raw json.RawMessage) {
			failures = 0
			var value any
			if json.Unmarshal(raw, &value) == nil {
				receive(value)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			if failures >= 2 {
				receive(map[string]any{"state": "service_unavailable", "error": err.Error()})
			}
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *PulseService) call(method string, params, out any) error {
	timeout := 2 * time.Minute
	if method == "stage_update" {
		timeout = 7 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := ipc.Call(ctx, p.socket, method, params, out); err != nil {
		return fmt.Errorf("pulse service: %w", err)
	}
	return nil
}
