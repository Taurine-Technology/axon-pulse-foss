//go:build windows

package state

// replaceFile uses MOVEFILE_WRITE_THROUGH on Windows, which flushes the
// same-volume replacement before returning.
func syncDirectory(string) error {
	return nil
}
