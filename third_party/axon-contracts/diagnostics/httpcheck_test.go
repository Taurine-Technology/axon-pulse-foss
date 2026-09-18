package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunHTTPCheckOKAndPhaseTimings(t *testing.T) {
	// Delay the response: on a loopback the whole request can finish in
	// under a millisecond and every ms-resolution phase timing rounds to
	// zero, so TTFB needs real time on the clock to be assertable.
	const responseDelay = 30 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(responseDelay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	result := RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets: []HTTPTarget{{URL: srv.URL, Method: "GET"}},
	})

	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	row := result.Results[0]
	if !row.OK || row.HTTPStatus != 200 {
		t.Fatalf("expected ok/200, got %+v", row)
	}
	if row.BytesRead != 5 {
		t.Fatalf("expected 5 body bytes, got %d", row.BytesRead)
	}
	if !row.TLSValid {
		t.Fatal("plain http must report tls_valid=true by contract")
	}
	// TTFB spans the handler's sleep; allow generous scheduling slop
	// below the configured delay but require a clearly-nonzero reading.
	if row.TTFBMS < 10 {
		t.Fatalf("expected TTFB >= 10ms (server delays %v), got %+v", responseDelay, row)
	}
	if row.TotalMS < row.TTFBMS {
		t.Fatalf("total must include TTFB, got %+v", row)
	}
}

func TestRunHTTPCheckBodyWindowCap(t *testing.T) {
	big := strings.Repeat("x", maxHTTPBodyBytes*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	result := RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets: []HTTPTarget{{URL: srv.URL, Method: "GET"}},
	})
	if got := result.Results[0].BytesRead; got != maxHTTPBodyBytes {
		t.Fatalf("body window not capped: read %d", got)
	}
}

func TestRunHTTPCheckExpectStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	result := RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets: []HTTPTarget{{URL: srv.URL, ExpectStatus: 200}},
	})
	row := result.Results[0]
	if row.OK {
		t.Fatalf("expected failure, got %+v", row)
	}
	if row.HTTPStatus != 503 || !strings.Contains(row.Error, "expected status 200") {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestRunHTTPCheckFollowsRedirectsBounded(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	hopper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer hopper.Close()

	result := RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets:         []HTTPTarget{{URL: hopper.URL}},
		FollowRedirects: true,
	})
	row := result.Results[0]
	if !row.OK || row.RedirectCount != 1 || row.HTTPStatus != 200 {
		t.Fatalf("redirect not followed cleanly: %+v", row)
	}

	// Without follow, the 302 itself is the (ok, 3xx) outcome.
	result = RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets: []HTTPTarget{{URL: hopper.URL}},
	})
	row = result.Results[0]
	if !row.OK || row.HTTPStatus != 302 || row.RedirectCount != 0 {
		t.Fatalf("expected raw 302 outcome, got %+v", row)
	}
}

func TestRunHTTPCheckTargetCapAndUnreachable(t *testing.T) {
	targets := make([]HTTPTarget, 0, maxHTTPTargets+5)
	for range maxHTTPTargets + 5 {
		// Reserved TEST-NET address: connection fails fast with the 1ms-ish
		// dial timeout budget below.
		targets = append(targets, HTTPTarget{URL: "http://192.0.2.1:81/x"})
	}
	result := RunHTTPCheck(context.Background(), HTTPCheckOptions{
		Targets:            targets,
		PerTargetTimeoutMS: 50,
		TotalBudgetMS:      400,
	})
	if len(result.Results) != maxHTTPTargets {
		t.Fatalf("target cap not applied: %d results", len(result.Results))
	}
	failed := 0
	for _, row := range result.Results {
		if !row.OK && row.Error != "" {
			failed++
		}
	}
	if failed != maxHTTPTargets {
		t.Fatalf("expected every target to fail, got %d/%d", failed, maxHTTPTargets)
	}
}

func TestRunCaptiveProbe(t *testing.T) {
	clean := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer clean.Close()
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>login required</html>"))
	}))
	defer portal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://portal.example.net/login", http.StatusFound)
	}))
	defer redirector.Close()

	outcome := runCaptiveProbe(context.Background(), clean.URL, nil)
	if !outcome.Attempted || outcome.Suspected {
		t.Fatalf("clean 204 flagged: %+v", outcome)
	}

	outcome = runCaptiveProbe(context.Background(), portal.URL, nil)
	if !outcome.Suspected {
		t.Fatalf("portal 200-with-body not flagged: %+v", outcome)
	}

	outcome = runCaptiveProbe(context.Background(), redirector.URL, nil)
	if !outcome.Suspected {
		t.Fatalf("off-host redirect not flagged: %+v", outcome)
	}
}
