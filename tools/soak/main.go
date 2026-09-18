// Command soak measures long-running Pulse RSS, CPU, queue, and data budgets.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type (
	result struct {
		DurationSeconds int64   `json:"duration_seconds"`
		Samples         int     `json:"samples"`
		MaxRSSBytes     int64   `json:"max_rss_bytes"`
		AverageCPU      float64 `json:"average_cpu_pct"`
		WithinRSSBudget bool    `json:"within_40_mib_rss_budget"`
		WithinCPUBudget bool    `json:"within_1_pct_cpu_budget"`
		Status          any     `json:"final_status,omitempty"`
	}
)

func main() {
	if err := run(); err != nil {
		fatal(err)
	}
}

func run() error {
	binary := flag.String("binary", "bin/axon-pulse", "headless Pulse binary")
	duration := flag.Duration("duration", 24*time.Hour, "soak duration")
	controller := flag.String("url", "", "optional controller URL")
	token := flag.String("token", "", "optional claim token")
	interval := flag.Duration("interval", 10*time.Second, "sampling interval")
	flag.Parse()
	if *interval <= 0 {
		return fmt.Errorf("--interval must be greater than zero")
	}
	stateDir, err := os.MkdirTemp("", "axon-pulse-soak-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stateDir) }()
	socket := filepath.Join(stateDir, "pulse.sock")
	command := exec.Command(*binary, "run", "--state-dir", stateDir, "--socket", socket)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	defer func() { _ = command.Process.Kill() }()
	time.Sleep(500 * time.Millisecond)
	if *controller != "" || *token != "" {
		if *controller == "" || *token == "" {
			return fmt.Errorf("--url and --token must be supplied together")
		}
		connect := exec.Command(*binary, "connect", "--state-dir", stateDir, "--socket", socket, "--url", *controller, "--token", *token)
		if output, err := connect.CombinedOutput(); err != nil {
			return fmt.Errorf("connect: %w: %s", err, output)
		}
	}
	started := time.Now()
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	deadline := time.NewTimer(*duration)
	defer deadline.Stop()
	var samples int
	var maxRSS int64
	var cpuTotal float64
	for {
		select {
		case <-ticker.C:
			rss, cpu, err := processSample(command.Process.Pid)
			if err != nil {
				return err
			}
			samples++
			maxRSS = max(maxRSS, rss)
			cpuTotal += cpu
		case <-deadline.C:
			status := exec.Command(*binary, "status", "--state-dir", stateDir, "--socket", socket)
			var finalStatus any
			output, err := status.Output()
			if err != nil {
				return fmt.Errorf("final status: %w", err)
			}
			if err := json.Unmarshal(output, &finalStatus); err != nil {
				return fmt.Errorf("decode final status: %w", err)
			}
			if err := command.Process.Signal(os.Interrupt); err != nil {
				return fmt.Errorf("stop Pulse: %w", err)
			}
			if err := command.Wait(); err != nil {
				return fmt.Errorf("wait for Pulse: %w", err)
			}
			average := 0.0
			if samples > 0 {
				average = cpuTotal / float64(samples)
			}
			final := result{DurationSeconds: int64(time.Since(started).Seconds()), Samples: samples, MaxRSSBytes: maxRSS, AverageCPU: average, WithinRSSBudget: maxRSS <= 40<<20, WithinCPUBudget: average < 1, Status: finalStatus}
			encoded, err := json.MarshalIndent(final, "", "  ")
			if err != nil {
				return fmt.Errorf("encode soak result: %w", err)
			}
			fmt.Println(string(encoded))
			if !final.WithinRSSBudget || !final.WithinCPUBudget {
				return fmt.Errorf("resource budget exceeded")
			}
			return nil
		}
	}
}

func processSample(pid int) (int64, float64, error) {
	output, err := exec.Command("ps", "-o", "rss=,%cpu=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("sample process: %w", err)
	}
	fields := bytes.Fields(output)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ps output %q", strings.TrimSpace(string(output)))
	}
	rssKiB, err := strconv.ParseInt(string(fields[0]), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	cpu, err := strconv.ParseFloat(string(fields[1]), 64)
	return rssKiB << 10, cpu, err
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "pulse-soak:", err)
	os.Exit(1)
}
