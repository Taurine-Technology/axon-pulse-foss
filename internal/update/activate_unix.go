//go:build !windows

package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Activate atomically replaces a headless executable, retains the previous
// binary, and rolls back if the staged binary cannot pass its version probe.
// The caller restarts only after this method succeeds.
func Activate(ctx context.Context, staged, executable string) error {
	if InstallMethod() == "apt" {
		return ErrPackageManaged
	}
	if !filepath.IsAbs(executable) {
		return errors.New("update executable path must be absolute")
	}
	info, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("inspect active executable: %w", err)
	}
	newPath := executable + ".pulse-new"
	previousPath := executable + ".pulse-previous"
	if err := copyExecutable(staged, newPath, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Remove(previousPath)
	if err := os.Rename(executable, previousPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("archive active executable: %w", err)
	}
	if err := os.Rename(newPath, executable); err != nil {
		_ = os.Rename(previousPath, executable)
		return fmt.Errorf("activate staged executable: %w", err)
	}
	healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(healthCtx, executable, "version").CombinedOutput(); err != nil {
		failedPath := executable + ".pulse-failed"
		_ = os.Remove(failedPath)
		moveFailed := os.Rename(executable, failedPath)
		rollback := os.Rename(previousPath, executable)
		return fmt.Errorf("new executable health check failed (%s): %w; rollback: %w", output, err, errors.Join(moveFailed, rollback))
	}
	return syncDirectory(filepath.Dir(executable))
}

func copyExecutable(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open staged executable: %w", err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create replacement executable: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	syncErr, closeErr := output.Sync(), output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("write replacement executable: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }() // durability comes from the checked Sync
	return directory.Sync()
}
