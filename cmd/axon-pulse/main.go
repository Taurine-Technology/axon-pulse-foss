// Command axon-pulse is the headless Pulse service and CLI entry point.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-contracts/gen/go/logging"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/service"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
	"github.com/Taurine-Technology/axon-pulse/internal/support"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "apply-update":
		if err := pulseupdate.ApplyPending(args[1:]); err != nil {
			fmt.Fprintln(stderr, "axon-pulse:", err)
			return 1
		}
		return 0
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	case "run":
		return runService(args[1:], stderr)
	case "connect":
		return connect(args[1:], os.Stdin, stdout, stderr)
	case "local":
		return setMode(append(append([]string(nil), args[1:]...), "standalone"), stdout, stderr)
	case "mode":
		return setMode(args[1:], stdout, stderr)
	case "channel":
		return updateChannel(args[1:], stdout, stderr)
	case "status":
		return status(args[1:], stdout, stderr)
	case "test":
		return runTest(args[1:], stdout, stderr)
	case "pause":
		return pause(args[1:], stdout, stderr)
	case "resume":
		return resume(args[1:], stdout, stderr)
	case "disconnect":
		return disconnect(args[1:], stdout, stderr)
	case "logs":
		return logs(args[1:], stdout, stderr)
	case "support-bundle":
		return supportBundle(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "axon-pulse: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Axon Pulse — subscriber-side network quality sensor

Usage:
  axon-pulse <command>

Commands:
  run        Run the always-on pulsed service
  connect    Claim this sensor with a controller invite
  local      Start or switch to private local-only measurement
  mode       Switch between standalone and connected operation
  channel    Show or choose the update stream (main, beta, alpha)
  status     Show service, upload, and spool status
  test       Run latency, DNS, and web checks now
  pause      Pause measurements (default: 1 hour)
  resume     Resume measurements
  disconnect Remove local controller credentials and queued data
  logs       Follow service logs on Linux
  support-bundle Export a bounded, redacted local diagnostic bundle
  version    Print build version
  help       Show this help
`)
}

func runService(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", "", "service state directory")
	socket := flags.String("socket", "", "local IPC socket path")
	windowsService := flags.Bool("windows-service", false, "run under the Windows Service Control Manager")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	dir, socketPath, err := paths(*stateDir, *socket)
	if err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		return 1
	}
	logger := logging.Setup("pulsed")
	daemon, err := service.Open(dir, socketPath, logger)
	if err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runErr := runPlatformService(ctx, daemon, *windowsService)
	closeErr := daemon.Close()
	if err := errors.Join(runErr, closeErr); err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		if _, restart := daemon.PackageRestart(); !restart {
			return 1
		}
	}
	if path, restart := daemon.PackageRestart(); restart {
		// APT replaced the package: re-exec in place so the PID (and the
		// systemd unit) continue with the new build.
		stop()
		fmt.Fprintln(stderr, "axon-pulse: restart into upgraded package failed:", service.ReexecInstalled(path))
		return 1
	}
	return 0
}

func connect(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controller := flags.String("url", "", "Axon controller URL")
	token := flags.String("token", "", "single-use or fleet claim token")
	tokenStdin := flags.Bool("token-stdin", false, "read the claim token from standard input instead of process arguments")
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *controller == "" || (*token == "" && !*tokenStdin) {
		fmt.Fprintln(stderr, "axon-pulse: connect requires --url and either --token or --token-stdin")
		return 2
	}
	if *tokenStdin {
		if *token != "" {
			fmt.Fprintln(stderr, "axon-pulse: --token and --token-stdin cannot be used together")
			return 2
		}
		value, err := readClaimToken(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "axon-pulse:", err)
			return 2
		}
		*token = value
	}
	var result service.Status
	if code := call(*stateDir, *socket, "connect", map[string]string{"url": *controller, "token": *token}, &result, stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "Connected to %s (sensor %s). State: %s\n", result.ControllerURL, result.SensorID, result.State)
	return 0
}

func updateChannel(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("channel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 || (flags.NArg() == 1 && !state.ValidUpdateChannel(flags.Arg(0))) {
		fmt.Fprintln(stderr, "axon-pulse: channel accepts main, beta, or alpha")
		return 2
	}
	if flags.NArg() == 0 {
		var current service.Status
		if code := call(*stateDir, *socket, "status", map[string]bool{"live": false}, &current, stderr); code != 0 {
			return code
		}
		if current.UpdateMethod == "apt" {
			fmt.Fprintln(stdout, "Updates are managed by APT; select the stream in your APT package sources")
			return 0
		}
		if current.UpdateChannelLocked {
			fmt.Fprintln(stdout, "Update stream: fixed by AXON_PULSE_UPDATE_INDEX_URL in the service environment")
			return 0
		}
		fmt.Fprintf(stdout, "Update stream: %s\n", current.UpdateChannel)
		return 0
	}
	var result service.UpdateStatus
	if code := call(*stateDir, *socket, "set_update_channel", map[string]string{"channel": flags.Arg(0)}, &result, stderr); code != 0 {
		return code
	}
	switch {
	case result.CheckError != "":
		fmt.Fprintf(stdout, "Update stream: %s. Check failed: %s\n", result.Channel, result.CheckError)
	case result.AvailableVersion != "":
		fmt.Fprintf(stdout, "Update stream: %s. Version %s is available; the service will install it on its next automatic check, or run the update from the app.\n", result.Channel, result.AvailableVersion)
	default:
		fmt.Fprintf(stdout, "Update stream: %s. Version %s is up to date.\n", result.Channel, result.CurrentVersion)
	}
	return 0
}

func status(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	live := flags.Bool("live", false, "include the in-memory latency ring")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var result service.Status
	if code := call(*stateDir, *socket, "status", map[string]bool{"live": *live}, &result, stderr); code != 0 {
		return code
	}
	return writeJSON(stdout, stderr, result)
}

func runTest(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	_ = flags.Bool("override", false, "deprecated: user-initiated tests always run; conditions are annotated")
	profile := flags.String("profile", "", "test profile: household (default), saturation, content or capacity")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *profile != "" && !protocol.KnownSpeedProfile(*profile) {
		fmt.Fprintln(stderr, "axon-pulse: --profile must be household, saturation, content or capacity")
		return 2
	}
	var result map[string]any
	if code := call(*stateDir, *socket, "test", map[string]any{"profile": *profile}, &result, stderr); code != 0 {
		return code
	}
	return writeJSON(stdout, stderr, result)
}

func setMode(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mode", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || (flags.Arg(0) != state.ModeStandalone && flags.Arg(0) != state.ModeConnected) {
		fmt.Fprintln(stderr, "axon-pulse: mode requires standalone or connected")
		return 2
	}
	var result service.Status
	if code := call(*stateDir, *socket, "set_mode", map[string]string{"mode": flags.Arg(0)}, &result, stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "Mode: %s. State: %s\n", result.Mode, result.State)
	return 0
}

func pause(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pause", flag.ContinueOnError)
	flags.SetOutput(stderr)
	duration := flags.Duration("for", time.Hour, "pause duration (maximum 168h)")
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *duration < 0 || *duration > 7*24*time.Hour {
		fmt.Fprintln(stderr, "axon-pulse: --for must be between 0 and 168h")
		return 2
	}
	var result service.Status
	if code := call(*stateDir, *socket, "pause", map[string]int64{"seconds": int64(duration.Seconds())}, &result, stderr); code != 0 {
		return code
	}
	fmt.Fprintf(stdout, "State: %s\n", result.State)
	return 0
}

func resume(args []string, stdout, stderr io.Writer) int {
	return pause(append([]string{"--for", "0"}, args...), stdout, stderr)
}

func disconnect(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if code := call(*stateDir, *socket, "disconnect", nil, nil, stderr); code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "Axon controller credentials and queued data were removed; Pulse is now local-only.")
	return 0
}

func logs(args []string, stdout, stderr io.Writer) int {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(stderr, "axon-pulse: logs is currently available on Linux")
		return 1
	}
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	follow := flags.Bool("follow", true, "follow new log entries")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	commandArgs := []string{"-u", "axon-pulse.service", "--no-pager", "-n", "100"}
	if *follow {
		commandArgs = append(commandArgs, "-f")
	}
	command := exec.Command("journalctl", commandArgs...)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		return 1
	}
	return 0
}

func supportBundle(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("out", "axon-pulse-support.json", "output JSON path")
	socket := flags.String("socket", "", "local IPC socket path")
	stateDir := flags.String("state-dir", "", "service state directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var result json.RawMessage
	if code := call(*stateDir, *socket, "support_bundle", nil, &result, stderr); code != 0 {
		return code
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, result, "", "  "); err != nil {
		fmt.Fprintln(stderr, "axon-pulse: encode support bundle:", err)
		return 1
	}
	formatted.WriteByte('\n')
	path := filepath.Clean(*output)
	if err := writePrivateFile(path, formatted.Bytes()); err != nil {
		fmt.Fprintln(stderr, "axon-pulse: write support bundle:", err)
		return 1
	}
	fmt.Fprintln(stdout, path)
	return 0
}

func writePrivateFile(path string, data []byte) error {
	return support.WritePrivateFile(path, data)
}

func call(stateDir, socket, method string, params, out any, stderr io.Writer) int {
	_, socketPath, err := paths(stateDir, socket)
	if err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ipc.Call(ctx, socketPath, method, params, out); err != nil {
		var ipcErr *ipc.Error
		if errors.As(err, &ipcErr) {
			fmt.Fprintf(stderr, "axon-pulse: %s (%s)\n", ipcErr.Message, ipcErr.Code)
		} else {
			fmt.Fprintln(stderr, "axon-pulse:", err)
		}
		return 1
	}
	return 0
}

func paths(configuredDir, configuredSocket string) (string, string, error) {
	dir := configuredDir
	var err error
	if dir == "" {
		dir, err = state.DefaultDir()
	}
	if err != nil {
		return "", "", err
	}
	socket := configuredSocket
	if socket == "" {
		socket = state.SocketPath(dir)
	}
	return dir, socket, nil
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, "axon-pulse:", err)
		return 1
	}
	return 0
}
