//go:build windows

package ipc

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

const (
	// Owner, LocalSystem, and Administrators receive full control. The protected
	// DACL prevents other interactive users from opening the credential-bearing
	// control pipe on multi-user Windows hosts.
	pipeSecurityDescriptor = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)"
)

func listenLocal(path string) (net.Listener, error) {
	return winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: pipeSecurityDescriptor,
		MessageMode:        true,
		InputBufferSize:    maxMessageBytes,
		OutputBufferSize:   maxMessageBytes,
	})
}

func dialLocal(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}

func cleanupLocal(string) error { return nil }
