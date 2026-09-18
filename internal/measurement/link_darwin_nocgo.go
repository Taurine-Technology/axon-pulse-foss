//go:build darwin && !cgo

package measurement

import (
	"errors"
)

// coreWLANContext requires cgo; static builds use the system_profiler parser.
func coreWLANContext(bool) (LocalContext, error) {
	return LocalContext{}, errors.New("corewlan collector requires cgo")
}
