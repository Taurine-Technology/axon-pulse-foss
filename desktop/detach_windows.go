//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func detach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}
