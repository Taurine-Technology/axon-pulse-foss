package buildinfo

import (
	"fmt"
)

// Injected at link time. Product lets each binary reuse the package without
// reporting itself as the switch agent.
var (
	Product = "axon"
	Version = "0.0.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns the human-readable version line used by --version,
// discovery payloads, and version responses.
func String() string {
	return fmt.Sprintf("%s %s (commit %s, built %s)", Product, Version, Commit, Date)
}

// UserAgent returns a stable HTTP user agent for one product component.
func UserAgent(component string) string {
	if component == "" {
		return fmt.Sprintf("%s/%s", Product, Version)
	}
	return fmt.Sprintf("%s/%s (%s)", Product, Version, component)
}
