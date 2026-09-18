//go:build !windows

package main

import (
	"context"
	"errors"

	"github.com/Taurine-Technology/axon-pulse/internal/service"
)

func runPlatformService(ctx context.Context, daemon *service.Service, windowsService bool) error {
	if windowsService {
		return errors.New("--windows-service is available only on Windows")
	}
	return daemon.Run(ctx)
}
