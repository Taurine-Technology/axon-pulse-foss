//go:build linux

package diagnostics

// ReadCPUBusy returns the Linux host's aggregate busy/total CPU jiffies.
func ReadCPUBusy() (CPUSample, bool) {
	return readCPUBusyFile("/proc/stat")
}
