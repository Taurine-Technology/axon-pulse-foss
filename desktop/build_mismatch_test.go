package main

import (
	"testing"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
)

func TestDesktopBuildMismatchOnlyForDifferentReleases(t *testing.T) {
	previous := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = previous })
	for _, tt := range []struct {
		gui, service string
		want         bool
	}{
		{"0.0.7-alpha.11", "0.0.7-alpha.11", false},
		{"0.0.7-alpha.11", "v0.0.7-alpha.11", false},
		{"0.0.7-alpha.11", "0.0.7-alpha.12", true},
		{"0.0.7-alpha.11", "", false},
		{"", "0.0.7-alpha.12", false},
		{"0.0.0-dev", "0.0.7-alpha.12", false},
		{"0.0.7-alpha.11", "0.0.0-dev", false},
	} {
		buildinfo.Version = tt.gui
		if got := desktopBuildMismatch(tt.service); got != tt.want {
			t.Errorf("desktopBuildMismatch(gui=%q, service=%q) = %v, want %v", tt.gui, tt.service, got, tt.want)
		}
	}
}
