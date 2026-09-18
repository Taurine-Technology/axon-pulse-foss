//go:build windows

package state

func isPrivileged() bool {
	return false
}
