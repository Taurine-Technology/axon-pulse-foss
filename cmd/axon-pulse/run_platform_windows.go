//go:build windows

package main

import (
	"context"
	"fmt"

	"github.com/Taurine-Technology/axon-pulse/internal/service"
	"golang.org/x/sys/windows/svc"
)

type (
	pulseServiceHandler struct {
		daemon *service.Service
	}
)

const (
	windowsServiceName = "AxonPulse"
)

func runPlatformService(ctx context.Context, daemon *service.Service, windowsService bool) error {
	if !windowsService {
		return daemon.Run(ctx)
	}
	if err := svc.Run(windowsServiceName, &pulseServiceHandler{daemon: daemon}); err != nil {
		return fmt.Errorf("run Windows service: %w", err)
	}
	return nil
}

func (h *pulseServiceHandler) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errors := make(chan error, 1)
	changes <- svc.Status{State: svc.StartPending}
	go func() { errors <- h.daemon.Run(ctx) }()
	status := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- status
	for {
		select {
		case err := <-errors:
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				if err := <-errors; err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}
