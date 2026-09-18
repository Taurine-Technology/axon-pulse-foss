//go:build !linux

package diagnostics

// ReadCPUBusy reports unavailable on platforms without Linux procfs. Callers
// retain the result and lower confidence instead of failing a measurement.
func ReadCPUBusy() (CPUSample, bool) {
	return CPUSample{}, false
}
