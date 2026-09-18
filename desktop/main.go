package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

type (
	trayStatus struct {
		State   string `json:"state"`
		Version string `json:"version"`
		Samples []struct {
			Success bool    `json:"success"`
			RTTMS   float64 `json:"rtt_ms"`
		} `json:"samples"`
		Live []struct {
			Success bool    `json:"success"`
			RTTMS   float64 `json:"rtt_ms"`
		} `json:"live"`
		Upload struct {
			LastSuccess time.Time `json:"last_success"`
		} `json:"upload"`
	}

	trayView struct {
		state      string
		label      string
		lastUpload string
	}
)

const (
	desktopStartURL = "/"
)

var (
	//go:embed all:frontend
	assets embed.FS

	// 22px white silhouette of the Taurine mark. Health stays in the tooltip and
	// menu text so the always-visible icon remains deliberately unobtrusive.
	//
	//go:embed tray-mask.png
	trayMask []byte
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--desktop-update-helper" {
		if err := pulseupdate.ApplyDesktopPending(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--health-check" {
		expectedVersion := ""
		if len(os.Args) > 2 {
			expectedVersion = os.Args[2]
		}
		if err := localHealthCheck(expectedVersion); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(buildinfo.String())
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func localHealthCheck(expectedVersion string) error {
	runningVersion := buildinfo.Version
	if runningVersion == "" {
		runningVersion = "0.0.0-dev"
	}
	if expectedVersion != "" && normalizedVersion(runningVersion) != normalizedVersion(expectedVersion) {
		return fmt.Errorf("desktop build version %q does not match expected update %q", runningVersion, expectedVersion)
	}
	stateDir, err := state.DefaultDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var status struct {
		Version string `json:"version"`
	}
	if err := ipc.Call(ctx, state.SocketPath(stateDir), "status", nil, &status); err != nil {
		return fmt.Errorf("local service health check: %w", err)
	}
	if status.Version == "" {
		return errors.New("local service health response omitted its version")
	}
	if expectedVersion != "" && normalizedVersion(status.Version) != normalizedVersion(expectedVersion) {
		return fmt.Errorf("local service version %q does not match expected update %q", status.Version, expectedVersion)
	}
	return nil
}

func normalizedVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// desktopBuildMismatch reports whether the running service is a different
// release than this window's build. Dev builds never count, mirroring the
// frontend's rule, so local development does not relaunch in a loop.
func desktopBuildMismatch(serviceVersion string) bool {
	gui := normalizedVersion(buildinfo.Version)
	service := normalizedVersion(serviceVersion)
	if gui == "" || service == "" || strings.Contains(gui, "dev") || strings.Contains(service, "dev") {
		return false
	}
	return gui != service
}

// run hosts the desktop app so deferred cleanup (signal handlers, service
// resources) executes before main exits on error; log.Fatal in main would
// otherwise skip the defers.
func run() error {
	stateDir, err := state.DefaultDir()
	if err != nil {
		return err
	}
	socketPath := state.SocketPath(stateDir)
	if len(os.Args) > 1 && os.Args[1] == "--service" {
		return runService(stateDir, socketPath)
	}
	ensureService(socketPath)
	pulse := NewPulseService(socketPath)
	for _, argument := range os.Args[1:] {
		if strings.HasPrefix(argument, "axon-pulse://") {
			_ = pulse.SetClaimURL(argument)
		}
	}

	var app *application.App
	var relaunching atomic.Bool
	app = application.New(application.Options{
		Name: "Axon Pulse", Description: "Private network quality monitoring",
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Services: []application.Service{application.NewService(pulse)},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: pulseupdate.DesktopInstanceID,
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				if index := slices.Index(data.Args, "--relaunch-after-update"); index >= 0 {
					// The helper names the replacement bundle; a bare flag
					// (older helper) falls back to this process's own path.
					bundle := ""
					if index+1 < len(data.Args) {
						bundle = data.Args[index+1]
					}
					if relaunching.CompareAndSwap(false, true) {
						ready, err := prepareDesktopRelaunch(bundle)
						if err != nil {
							log.Printf("relaunch updated desktop: %v", err)
						}
						if ready {
							app.Quit()
						} else {
							relaunching.Store(false)
						}
					}
					return
				}
				for _, argument := range data.Args {
					if strings.HasPrefix(argument, "axon-pulse://") && pulse.SetClaimURL(argument) == nil {
						showWindow(app)
						emitToWindow(app, "pulse:claim", pulse.PendingClaim())
					}
				}
			},
		},
		Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false, ActivationPolicy: application.ActivationPolicyAccessory},
	})
	pulse.configureDialogs(app.Dialog)
	// A user-driven relaunch reuses the update hand-off: spawn the detached
	// relauncher, then quit so the new instance can take the single-instance
	// lock. The service is unaffected.
	pulse.relaunch = func() error {
		if !relaunching.CompareAndSwap(false, true) {
			return nil
		}
		ready, err := prepareDesktopRelaunch("")
		if err == nil && !ready {
			err = errors.New("relaunch is not supported on this platform; close and reopen Axon Pulse")
		}
		if err != nil {
			relaunching.Store(false)
			return err
		}
		app.Quit()
		return nil
	}
	pulse.configureAutostart(app.Autostart)
	if err := pulse.initializeAutostart(stateDir); err != nil {
		log.Printf("autostart unavailable: %v", err)
	}
	newWindow(app)

	tray := app.SystemTray.New()
	configureTrayIcon(tray)
	tray.SetTooltip("Axon Pulse")
	menu := app.NewMenu()
	statusItem := menu.Add("Pulse is starting…").SetEnabled(false)
	lastUploadItem := menu.Add("No successful upload yet").SetEnabled(false)
	menu.Add("Open Pulse").OnClick(func(_ *application.Context) { showWindow(app) })
	menu.Add("Run speed test").OnClick(func(_ *application.Context) {
		showWindow(app)
		// An empty profile lets the service pick its default: household when
		// the controller supports it, otherwise the legacy content profile.
		go func() { _, _ = pulse.RunTest("") }()
	})
	var trayPaused atomic.Bool
	pauseItem := menu.Add("Pause for 1 hour")
	pauseItem.OnClick(func(_ *application.Context) {
		seconds := int64(3600)
		if trayPaused.Load() {
			seconds = 0
		}
		go func() { _, _ = pulse.Pause(seconds) }()
	})
	menu.Add("Pause until tomorrow").OnClick(func(_ *application.Context) {
		now := time.Now()
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		go func() { _, _ = pulse.Pause(int64(time.Until(tomorrow).Seconds())) }()
	})
	menu.AddSeparator()
	menu.Add("Quit desktop (service keeps running)").OnClick(func(_ *application.Context) { app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(func() { showWindow(app) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		app.Quit()
	}()
	go pulse.Watch(ctx, func(update any) {
		emitToWindow(app, "pulse:status", update)
		encoded, _ := json.Marshal(update)
		var status trayStatus
		if json.Unmarshal(encoded, &status) == nil && status.State != "" {
			// After a package upgrade the service re-execs itself into the
			// new build; this tray process is still the old one. With no
			// window open nobody is looking, so relaunch silently. With a
			// window open the Settings screen offers Restart Pulse instead.
			if desktopBuildMismatch(status.Version) {
				if _, open := app.Window.GetByName("main"); !open {
					if err := pulse.RelaunchDesktop(); err != nil {
						log.Printf("relaunch after package upgrade: %v", err)
					}
				}
			}
			view := trayViewFor(status)
			trayPaused.Store(view.state == "paused")
			// MenuItem.SetLabel does not dispatch to the UI thread on every
			// platform; SystemTray methods do so internally.
			application.InvokeSync(func() {
				statusItem.SetLabel("Pulse: " + view.label)
				lastUploadItem.SetLabel(view.lastUpload)
				if view.state == "paused" {
					pauseItem.SetLabel("Resume measurement")
				} else {
					pauseItem.SetLabel("Pause for 1 hour")
				}
			})
			tray.SetTooltip("Axon Pulse — " + view.label)
		}
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(event *application.ApplicationEvent) {
		if pulse.SetClaimURL(event.Context().URL()) == nil {
			showWindow(app)
			emitToWindow(app, "pulse:claim", pulse.PendingClaim())
		}
	})
	return app.Run()
}

func trayViewFor(status trayStatus) trayView {
	state, label := "needs_attention", "Needs attention"
	switch status.State {
	case "active":
		samples := status.Samples
		if len(samples) == 0 {
			samples = status.Live
		}
		if len(samples) == 0 {
			state, label = "needs_attention", "Checking"
			break
		}
		state, label = "healthy", "Healthy"
		failures := 0
		highLatency := false
		for _, sample := range samples[max(0, len(samples)-10):] {
			if !sample.Success {
				failures++
			} else if sample.RTTMS >= 180 {
				highLatency = true
			}
		}
		if failures >= 2 || highLatency {
			state, label = "degraded", "Degraded"
		}
	case "offline", "service_unavailable":
		state, label = "offline", "Offline"
	case "paused":
		state, label = "paused", "Paused"
	}
	lastUpload := "No successful upload yet"
	if !status.Upload.LastSuccess.IsZero() {
		lastUpload = "Last upload: " + status.Upload.LastSuccess.Local().Format("Jan 2, 15:04")
	}
	return trayView{state: state, label: label, lastUpload: lastUpload}
}

// configureTrayIcon keeps the monochrome mark legible on both light and dark
// menu bars: macOS tints a template image itself, Windows switches between the
// light/dark pair, and Linux panels are conventionally dark.
func configureTrayIcon(tray *application.SystemTray) {
	white := renderTrayIcon(color.RGBA{R: 255, G: 255, B: 255, A: 255})
	if white == nil {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		tray.SetTemplateIcon(white)
	case "windows":
		tray.SetIcon(renderTrayIcon(color.RGBA{R: 32, G: 34, B: 54, A: 255}))
		tray.SetDarkModeIcon(white)
	default:
		tray.SetIcon(white)
	}
}

func renderTrayIcon(ink color.RGBA) []byte {
	mask, err := png.Decode(bytes.NewReader(trayMask))
	if err != nil {
		return nil
	}
	bounds := mask.Bounds()
	canvas := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := mask.At(x, y).RGBA()
			if alpha > 0 {
				canvas.SetRGBA(x, y, color.RGBA{R: ink.R, G: ink.G, B: ink.B, A: uint8(alpha >> 8)})
			}
		}
	}
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, canvas)
	return encoded.Bytes()
}

func newWindow(app *application.App) application.Window {
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "main", Title: "Axon Pulse", URL: desktopStartURL,
		Width: 940, Height: 680, MinWidth: 760, MinHeight: 560,
		// Axon navy (matches the dark-theme --background token) so launch doesn't flash white.
		BackgroundColour: application.NewRGB(2, 4, 24),
	})
	// Allow a full close so the webview releases its memory and CPU. The tray
	// stays native-only and constructs a fresh viewer when the user opens it.
	window.RegisterHook(events.Common.WindowClosing, func(_ *application.WindowEvent) {
		app.Window.Remove(window.ID())
	})
	return window
}

func showWindow(app *application.App) {
	window, ok := app.Window.GetByName("main")
	if !ok {
		window = newWindow(app)
	}
	window.Show()
	window.Focus()
}

func emitToWindow(app *application.App, name string, data any) {
	if window, ok := app.Window.GetByName("main"); ok {
		window.EmitEvent(name, data)
	}
}
