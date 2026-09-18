package update

import (
	"os"
	"strings"
	"testing"
)

func TestInstalledExecutableMatchesTheLivePath(t *testing.T) {
	got, err := InstalledExecutable()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.Executable()
	if got != strings.TrimSuffix(want, deletedMarker) || strings.HasSuffix(got, deletedMarker) {
		t.Fatalf("InstalledExecutable = %q, os.Executable = %q", got, want)
	}
}
