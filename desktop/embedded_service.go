//go:build !externalservice

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/logging"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/service"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
)

func runService(stateDir, socketPath string) error {
	daemon, err := service.Open(stateDir, socketPath, logging.Setup("pulsed"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runErr := errors.Join(daemon.Run(ctx), daemon.Close())
	if path, restart := daemon.PackageRestart(); restart {
		// APT replaced the package: become the new build in place. Nothing
		// else restarts a desktop's detached service, so re-exec even after a
		// drain error rather than leave monitoring stopped.
		if runErr != nil {
			log.Printf("restarting into upgraded package despite shutdown error: %v", runErr)
		}
		stop()
		return errors.Join(runErr, service.ReexecInstalled(path))
	}
	return runErr
}

func ensureService(socketPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if ipc.Call(ctx, socketPath, "status", nil, nil) == nil {
		return
	}
	// The installed path, not os.Executable: after an APT upgrade the latter
	// names an unlinked inode and the spawn would fail.
	executable, err := pulseupdate.InstalledExecutable()
	if err != nil {
		log.Printf("locate Pulse service: %v", err)
		return
	}
	command := exec.Command(executable, "--service")
	detach(command)
	if err := command.Start(); err != nil {
		log.Printf("start Pulse service: %v", err)
	}
}
