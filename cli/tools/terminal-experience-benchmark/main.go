//go:build darwin || linux

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
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type sample struct {
	Transport string  `json:"transport"`
	Run       int     `json:"run"`
	Bytes     int64   `json:"bytes"`
	Events    int     `json:"events"`
	BytesSec  float64 `json:"bytes_per_second"`
	GapP50MS  float64 `json:"gap_p50_ms"`
	GapP95MS  float64 `json:"gap_p95_ms"`
	GapP99MS  float64 `json:"gap_p99_ms"`
	MaxGapMS  float64 `json:"max_gap_ms"`
	Over16MS  int     `json:"gaps_over_16_ms"`
	Over33MS  int     `json:"gaps_over_33_ms"`
	Over50MS  int     `json:"gaps_over_50_ms"`
	Over100MS int     `json:"gaps_over_100_ms"`
}

type aggregate struct {
	Summary        bool    `json:"summary"`
	Transport      string  `json:"transport"`
	Runs           int     `json:"runs"`
	MeanBytesSec   float64 `json:"mean_bytes_per_second"`
	MedianP95MS    float64 `json:"median_gap_p95_ms"`
	MedianP99MS    float64 `json:"median_gap_p99_ms"`
	MedianMaxMS    float64 `json:"median_max_gap_ms"`
	WorstGapMS     float64 `json:"worst_gap_ms"`
	TotalOver33MS  int     `json:"total_gaps_over_33_ms"`
	TotalOver50MS  int     `json:"total_gaps_over_50_ms"`
	TotalOver100MS int     `json:"total_gaps_over_100_ms"`
}

type readEvent struct {
	at time.Time
	n  int
}

type config struct {
	duration  time.Duration
	warmup    time.Duration
	pbStartup time.Duration
	runs      int
	pb        string
	target    string
	sshHost   string
	sshKey    string
	sshPort   int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "terminal experience benchmark:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("terminal-experience-benchmark", flag.ContinueOnError)
	cfg := config{}
	flags.DurationVar(&cfg.duration, "duration", 10*time.Second, "measured cmatrix duration per run")
	flags.DurationVar(&cfg.warmup, "warmup", 2*time.Second, "unmeasured cmatrix warmup")
	flags.DurationVar(&cfg.pbStartup, "pb-startup", 4*time.Second, "time allowed for pb to attach before starting cmatrix")
	flags.IntVar(&cfg.runs, "runs", 3, "runs per transport")
	flags.StringVar(&cfg.pb, "pb", "pb", "pb executable")
	flags.StringVar(&cfg.target, "target", "hn", "Paperboat environment")
	flags.StringVar(&cfg.sshHost, "ssh-host", "root@157.180.74.88", "SSH destination")
	flags.StringVar(&cfg.sshKey, "ssh-key", "", "SSH private key")
	flags.IntVar(&cfg.sshPort, "ssh-port", 22, "SSH port")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			var help bytes.Buffer
			flags.SetOutput(&help)
			flags.Usage()
			if _, writeErr := io.Copy(output, &help); writeErr != nil {
				return newBenchmarkFailure("write benchmark help", writeErr)
			}
			return nil
		}
		return newBenchmarkFailure("invalid benchmark arguments", err)
	}
	if cfg.duration < time.Second || cfg.warmup < 0 || cfg.pbStartup < time.Second || cfg.runs < 1 || cfg.runs > 20 || cfg.pb == "" || cfg.target == "" || cfg.sshHost == "" || cfg.sshPort < 1 || cfg.sshPort > 65535 {
		return errors.New("invalid benchmark configuration")
	}

	encoder := json.NewEncoder(output)
	results := make(map[string][]sample)
	for runIndex := 1; runIndex <= cfg.runs; runIndex++ {
		transports := []string{"ssh", "pb_quic", "pb_wss"}
		offset := (runIndex - 1) % len(transports)
		order := append(append([]string{}, transports[offset:]...), transports[:offset]...)
		for _, transport := range order {
			result, err := measure(ctx, cfg, transport, runIndex)
			if err != nil {
				return fmt.Errorf("%s run %d: %w", transport, runIndex, err)
			}
			if err := encoder.Encode(result); err != nil {
				return newBenchmarkFailure("write benchmark output", err)
			}
			results[transport] = append(results[transport], result)
		}
	}
	for _, transport := range []string{"ssh", "pb_quic", "pb_wss"} {
		if err := encoder.Encode(summarizeRuns(transport, results[transport])); err != nil {
			return newBenchmarkFailure("write benchmark summary", err)
		}
	}
	return nil
}

func measure(parent context.Context, cfg config, transport string, runIndex int) (result sample, resultErr error) {
	ctx, cancel := context.WithCancel(parent)
	cmatrix := "exec env TERM=xterm-256color cmatrix -n -u 2"
	var command *exec.Cmd
	pbSession := ""
	if transport == "ssh" {
		args := []string{"-tt", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-p", fmt.Sprint(cfg.sshPort)}
		if cfg.sshKey != "" {
			args = append(args, "-i", cfg.sshKey)
		}
		args = append(args, cfg.sshHost, cmatrix)
		command = exec.CommandContext(ctx, "ssh", args...)
	} else {
		pbSession = fmt.Sprintf("cmatrix-bench-%s-%d-%d", transport[3:], time.Now().UnixNano(), runIndex)
		command = exec.CommandContext(ctx, cfg.pb, "connect", cfg.target, "new", "--path", paperboatPath(transport), "--name", pbSession, "--status-bar", "off")
	}
	contextKillSucceeded := &atomic.Bool{}
	if cancelProcess := command.Cancel; cancelProcess != nil {
		command.Cancel = func() error {
			err := cancelProcess()
			if err == nil {
				contextKillSucceeded.Store(true)
			}
			return err
		}
	}
	var terminal *os.File
	var processDone chan error
	var readerDone chan struct{}
	var readStop chan struct{}
	processWaited := false
	processWaitErr := error(nil)
	processWaitIncluded := false
	forcedStop := false
	defer func() {
		if !processWaited {
			forcedStop = true
			cancel()
		}
		if readStop != nil {
			close(readStop)
		}
		if terminal != nil {
			if err := terminal.Close(); err != nil {
				resultErr = joinBenchmarkFailure(resultErr, "benchmark cleanup failed", err)
			}
		}
		if processDone != nil && !processWaited {
			select {
			case processWaitErr = <-processDone:
				processWaited = true
			case <-time.After(2 * time.Second):
				resultErr = joinBenchmarkFailure(resultErr, "benchmark process did not stop", context.DeadlineExceeded)
			}
		}
		if processWaitErr != nil && !processWaitIncluded && !(forcedStop && killedByContext(processWaitErr, contextKillSucceeded.Load())) {
			resultErr = joinBenchmarkFailure(resultErr, "benchmark process failed", processWaitErr)
		}
		if readerDone != nil {
			select {
			case <-readerDone:
			case <-time.After(2 * time.Second):
				resultErr = joinBenchmarkFailure(resultErr, "benchmark output reader did not stop", context.DeadlineExceeded)
			}
		}
		cancel()
		if pbSession != "" {
			if err := deletePBSession(cfg.pb, cfg.target, pbSession); err != nil {
				resultErr = joinBenchmarkFailure(resultErr, "Paperboat session cleanup failed", err)
			}
		}
	}()
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return sample{}, newBenchmarkFailure("start benchmark process", err)
	}
	processDone = make(chan error, 1)
	go func() { processDone <- command.Wait() }()

	events := make(chan readEvent, 4096)
	readDone := make(chan error, 1)
	readStop = make(chan struct{})
	readerDone = make(chan struct{})
	go func() {
		defer close(readerDone)
		buffer := make([]byte, 32*1024)
		for {
			n, readErr := terminal.Read(buffer)
			if n > 0 {
				select {
				case events <- readEvent{at: time.Now(), n: n}:
				case <-readStop:
					return
				case <-ctx.Done():
					return
				}
			}
			if readErr != nil {
				readDone <- readErr
				return
			}
		}
	}()

	if pbSession != "" {
		select {
		case <-time.After(cfg.pbStartup):
		case err := <-readDone:
			return sample{}, newBenchmarkFailure("wait for Paperboat terminal output", err)
		case err := <-processDone:
			processWaited = true
			processWaitErr = err
			processWaitIncluded = true
			return sample{}, newBenchmarkFailure("Paperboat terminal exited during startup", err)
		case <-ctx.Done():
			return sample{}, newBenchmarkFailure("benchmark canceled during startup", ctx.Err())
		}
		if _, err := io.WriteString(terminal, cmatrix+"\n"); err != nil {
			return sample{}, newBenchmarkFailure("start remote benchmark command", err)
		}
	}
	if processErr, exited, err := waitForBenchmark(ctx, cfg.warmup, processDone); err != nil {
		return sample{}, newBenchmarkFailure("wait for benchmark warmup", err)
	} else if exited {
		processWaited = true
		processWaitErr = processErr
		processWaitIncluded = true
		return sample{}, newBenchmarkFailure("benchmark process exited during warmup", processErr)
	}
	for len(events) > 0 {
		<-events
	}
	started := time.Now()
	if processErr, exited, err := waitForBenchmark(ctx, cfg.duration, processDone); err != nil {
		return sample{}, newBenchmarkFailure("wait for benchmark measurement", err)
	} else if exited {
		processWaited = true
		processWaitErr = processErr
		processWaitIncluded = true
		return sample{}, newBenchmarkFailure("benchmark process exited before measurement completed", processErr)
	}
	finished := time.Now()
	select {
	case processWaitErr = <-processDone:
		processWaited = true
		processWaitIncluded = true
		return sample{}, newBenchmarkFailure("benchmark process exited before interrupt", processWaitErr)
	default:
	}
	if _, err := terminal.Write([]byte{3}); err != nil {
		return sample{}, newBenchmarkFailure("stop benchmark process", err)
	}
	select {
	case processWaitErr = <-processDone:
		processWaited = true
	case <-time.After(2 * time.Second):
		forcedStop = true
		return sample{}, newBenchmarkFailure("benchmark process did not stop after interrupt", context.DeadlineExceeded)
	}
	if processWaitErr != nil && !expectedBenchmarkInterrupt(processWaitErr) {
		processWaitIncluded = true
		return sample{}, newBenchmarkFailure("benchmark process failed after interrupt", processWaitErr)
	}
	processWaitIncluded = true

	var measured []readEvent
	for len(events) > 0 {
		event := <-events
		if !event.at.Before(started) && !event.at.After(finished) {
			measured = append(measured, event)
		}
	}
	if len(measured) < 2 {
		return sample{}, errors.New("benchmark command produced no measurable output")
	}
	return summarize(transport, runIndex, finished.Sub(started), measured), nil
}

type benchmarkFailure struct {
	message string
	causes  []error
}

func (e *benchmarkFailure) Error() string { return e.message }

func (e *benchmarkFailure) Unwrap() []error { return e.causes }

func newBenchmarkFailure(message string, causes ...error) error {
	kept := causes[:0]
	for _, cause := range causes {
		if cause != nil {
			kept = append(kept, cause)
		}
	}
	if len(kept) == 0 {
		return errors.New(message)
	}
	return &benchmarkFailure{message: message, causes: kept}
}

func joinBenchmarkFailure(primary error, message string, cause error) error {
	if cause == nil {
		return primary
	}
	if primary == nil {
		return newBenchmarkFailure(message, cause)
	}
	return newBenchmarkFailure(primary.Error()+"; cleanup failed", primary, cause)
}

func expectedBenchmarkInterrupt(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	if exit.ExitCode() == 130 {
		return true
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGINT
}

func killedByContext(err error, requestSucceeded bool) bool {
	if !requestSucceeded {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func paperboatPath(transport string) string {
	switch transport {
	case "pb_quic":
		return "q"
	case "pb_wss":
		return "w"
	default:
		return ""
	}
}

func deletePBSession(pb, target, session string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := closePBSession(ctx, pb, target, session); err != nil {
		return err
	}
	preview, err := exec.CommandContext(ctx, pb, "session", "delete", target, session, "--json").Output()
	if err == nil {
		return nil
	}
	var confirmation struct {
		Data struct {
			Token string `json:"confirmation_token"`
		} `json:"data"`
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		return newBenchmarkFailure("delete Paperboat session", err)
	}
	if err := json.Unmarshal(preview, &confirmation); err != nil {
		return newBenchmarkFailure("read Paperboat session deletion confirmation", err)
	}
	if confirmation.Data.Token == "" {
		return errors.New("Paperboat session deletion requires confirmation but returned no token")
	}
	if err := exec.CommandContext(ctx, pb, "session", "delete", target, session, "--confirm", confirmation.Data.Token, "--json").Run(); err != nil {
		return newBenchmarkFailure("confirm Paperboat session deletion", err)
	}
	return nil
}

func closePBSession(ctx context.Context, pb, target, session string) error {
	args := []string{"session", "close", target, session, "--json"}
	output, err := exec.CommandContext(ctx, pb, args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			return newBenchmarkFailure("close Paperboat session", err)
		}
		var confirmation struct {
			Data struct {
				Token string `json:"confirmation_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(output, &confirmation); err != nil {
			return newBenchmarkFailure("read Paperboat session close confirmation", err)
		}
		if confirmation.Data.Token == "" {
			return errors.New("Paperboat session close requires confirmation but returned no token")
		}
		args = append(args, "--confirm", confirmation.Data.Token)
		output, err = exec.CommandContext(ctx, pb, args...).Output()
		if err != nil {
			return newBenchmarkFailure("confirm Paperboat session close", err)
		}
	}
	var result struct {
		State          string `json:"state"`
		OperationState string `json:"operation_state"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return newBenchmarkFailure("read Paperboat session close result", err)
	}
	if result.State != "closed" || result.OperationState != "applied" {
		return errors.New("Paperboat session close is pending; session record was left intact")
	}
	return nil
}

func waitForBenchmark(ctx context.Context, duration time.Duration, processDone <-chan error) (processErr error, exited bool, waitErr error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil, false, nil
	case processErr := <-processDone:
		return processErr, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func summarize(transport string, runIndex int, duration time.Duration, events []readEvent) sample {
	result := sample{Transport: transport, Run: runIndex, Events: len(events)}
	for _, event := range events {
		result.Bytes += int64(event.n)
	}
	result.BytesSec = float64(result.Bytes) / duration.Seconds()
	if len(events) < 2 {
		return result
	}
	gaps := make([]time.Duration, 0, len(events)-1)
	for index := 1; index < len(events); index++ {
		gap := events[index].at.Sub(events[index-1].at)
		gaps = append(gaps, gap)
		if gap > 16*time.Millisecond {
			result.Over16MS++
		}
		if gap > 33*time.Millisecond {
			result.Over33MS++
		}
		if gap > 50*time.Millisecond {
			result.Over50MS++
		}
		if gap > 100*time.Millisecond {
			result.Over100MS++
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	result.GapP50MS = milliseconds(percentile(gaps, 50))
	result.GapP95MS = milliseconds(percentile(gaps, 95))
	result.GapP99MS = milliseconds(percentile(gaps, 99))
	result.MaxGapMS = milliseconds(gaps[len(gaps)-1])
	return result
}

func summarizeRuns(transport string, samples []sample) aggregate {
	result := aggregate{Summary: true, Transport: transport, Runs: len(samples)}
	p95 := make([]float64, 0, len(samples))
	p99 := make([]float64, 0, len(samples))
	maximums := make([]float64, 0, len(samples))
	for _, value := range samples {
		result.MeanBytesSec += value.BytesSec
		result.TotalOver33MS += value.Over33MS
		result.TotalOver50MS += value.Over50MS
		result.TotalOver100MS += value.Over100MS
		p95 = append(p95, value.GapP95MS)
		p99 = append(p99, value.GapP99MS)
		maximums = append(maximums, value.MaxGapMS)
		if value.MaxGapMS > result.WorstGapMS {
			result.WorstGapMS = value.MaxGapMS
		}
	}
	if len(samples) == 0 {
		return result
	}
	result.MeanBytesSec /= float64(len(samples))
	result.MedianP95MS = medianFloat(p95)
	result.MedianP99MS = medianFloat(p99)
	result.MedianMaxMS = medianFloat(maximums)
	return result
}

func medianFloat(values []float64) float64 {
	sort.Float64s(values)
	if len(values) == 0 {
		return 0
	}
	return values[(len(values)-1)/2]
}

func percentile(values []time.Duration, percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := (len(values)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}

func milliseconds(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}
