package main

import (
	"bytes"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestBundledDesktopAssetsResolveFromWindowStartURL(t *testing.T) {
	t.Parallel()
	if desktopStartURL != "/" {
		t.Fatalf("desktop start URL = %q, want root", desktopStartURL)
	}

	handler := application.BundledAssetFileServer(assets)
	for _, test := range []struct {
		path     string
		contains string
	}{
		{path: desktopStartURL, contains: "<title>Axon Pulse</title>"},
		{path: desktopStartURL, contains: "Use Pulse locally"},
		{path: desktopStartURL, contains: `<dialog id="onboarding"`},
		{path: desktopStartURL, contains: `aria-live="polite"`},
		{path: "/styles.css", contains: "--background:"},
		{path: "/styles.css", contains: "@media (max-width:520px)"},
		{path: "/styles.css", contains: "prefers-reduced-motion:reduce"},
		{path: "/styles.css", contains: "prefers-contrast:more"},
		{path: "/styles.css", contains: "manrope-latin.woff2"},
		{path: "/styles.css", contains: "plexmono-500-latin.woff2"},
		{path: "/manrope-latin.woff2", contains: "wOF2"},
		{path: "/plexmono-400-latin.woff2", contains: "wOF2"},
		{path: "/plexmono-500-latin.woff2", contains: "wOF2"},
		{path: "/plexmono-600-latin.woff2", contains: "wOF2"},
		{path: "/logo-mark.svg", contains: "<svg"},
		{path: "/app.js", contains: "PulseService"},
		{path: "/app.js", contains: "measurement_confidence"},
		{path: "/app.js", contains: "showModal()"},
		{path: "/app.js", contains: "addEventListener('cancel'"},
		{path: "/metrics.mjs", contains: "rollingStats"},
		{path: "/updates.mjs", contains: "waitForUpdateRestart"},
		{path: "/wails/runtime.js", contains: "window.wails"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			result := response.Result()
			defer func() { _ = result.Body.Close() }()
			if result.StatusCode != http.StatusOK {
				t.Fatalf("GET %s returned HTTP %d", test.path, result.StatusCode)
			}
			body, err := io.ReadAll(result.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), test.contains) {
				t.Fatalf("GET %s did not contain %q", test.path, test.contains)
			}
		})
	}
}

func TestTrayPresentationCoversHealthAndBackgroundStates(t *testing.T) {
	t.Parallel()
	status := trayStatus{State: "active"}
	status.Upload.LastSuccess = time.Date(2026, 8, 18, 12, 0, 0, 0, time.Local)
	if view := trayViewFor(status); view.state != "needs_attention" || view.label != "Checking" || !strings.Contains(view.lastUpload, "Last upload") {
		t.Fatalf("empty-evidence tray view = %+v", view)
	}
	status.Samples = make([]struct {
		Success bool    `json:"success"`
		RTTMS   float64 `json:"rtt_ms"`
	}, 3)
	for index := range status.Samples {
		status.Samples[index].Success = true
	}
	if view := trayViewFor(status); view.state != "healthy" {
		t.Fatalf("healthy tray view = %+v", view)
	}
	status.Samples[0].RTTMS = 200
	if view := trayViewFor(status); view.state != "degraded" {
		t.Fatalf("high-latency tray view = %+v", view)
	}
	status.Samples[0].RTTMS = 0
	status.Samples[1].Success, status.Samples[2].Success = false, false
	if view := trayViewFor(status); view.state != "degraded" {
		t.Fatalf("failed-sample tray view = %+v", view)
	}
	for serviceState, trayState := range map[string]string{"paused": "paused", "offline": "offline", "service_unavailable": "offline", "setup": "needs_attention", "captive": "needs_attention"} {
		status.State, status.Samples = serviceState, nil
		if view := trayViewFor(status); view.state != trayState {
			t.Fatalf("%s tray state = %+v", serviceState, view)
		}
	}
}

func TestTrayIconRemainsMonochrome(t *testing.T) {
	t.Parallel()
	white := renderTrayIcon(color.RGBA{R: 255, G: 255, B: 255, A: 255})
	if len(white) == 0 {
		t.Fatal("tray icon failed to render")
	}
	decoded, err := png.Decode(bytes.NewReader(white))
	if err != nil {
		t.Fatal(err)
	}
	opaque := 0
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			r, g, b, a := decoded.At(x, y).RGBA()
			if a != 0 {
				opaque++
			}
			if a != 0 && (r != g || g != b) {
				t.Fatalf("tray pixel at %d,%d is not monochrome", x, y)
			}
		}
	}
	if opaque == 0 {
		t.Fatal("tray icon rendered fully transparent")
	}
}
