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

type config struct {
	runs, samples int
	startup       time.Duration
	timeout       time.Duration
	interval      time.Duration
	pb, target    string
	sshHost       string
	sshKey        string
	sshPort       int
}

type result struct {
	Transport string  `json:"transport"`
	Run       int     `json:"run"`
	Samples   int     `json:"samples"`
	P50MS     float64 `json:"input_p50_ms"`
	P95MS     float64 `json:"input_p95_ms"`
	P99MS     float64 `json:"input_p99_ms"`
	MaxMS     float64 `json:"input_max_ms"`
	Over200MS int     `json:"responses_over_200_ms"`
	Over500MS int     `json:"responses_over_500_ms"`
}

type aggregate struct {
	Summary        bool    `json:"summary"`
	Transport      string  `json:"transport"`
	Runs           int     `json:"runs"`
	Samples        int     `json:"samples"`
	MedianP50MS    float64 `json:"median_input_p50_ms"`
	MedianP95MS    float64 `json:"median_input_p95_ms"`
	MedianP99MS    float64 `json:"median_input_p99_ms"`
	MedianMaxMS    float64 `json:"median_input_max_ms"`
	WorstMS        float64 `json:"worst_input_ms"`
	TotalOver200MS int     `json:"total_responses_over_200_ms"`
	TotalOver500MS int     `json:"total_responses_over_500_ms"`
}

var errResponseTimeout = errors.New("response timeout")

type inputBenchmarkFailure struct {
	message string
	cause   error
}

func (e inputBenchmarkFailure) Error() string { return e.message }

func (e inputBenchmarkFailure) Unwrap() error { return e.cause }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "terminal input benchmark:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("terminal-input-benchmark", flag.ContinueOnError)
	flagOutput := &bytes.Buffer{}
	flags.SetOutput(flagOutput)
	cfg := config{}
	flags.IntVar(&cfg.runs, "runs", 10, "runs per transport")
	flags.IntVar(&cfg.samples, "samples", 50, "raw key responses per run")
	flags.DurationVar(&cfg.startup, "pb-startup", 3*time.Second, "time allowed for pb to attach")
	flags.DurationVar(&cfg.timeout, "timeout", 3*time.Second, "timeout per key response")
	flags.DurationVar(&cfg.interval, "interval", 20*time.Millisecond, "delay between keys")
	flags.StringVar(&cfg.pb, "pb", "pb", "pb executable")
	flags.StringVar(&cfg.target, "target", "hn", "Paperboat environment")
	flags.StringVar(&cfg.sshHost, "ssh-host", "root@157.180.74.88", "SSH destination")
	flags.StringVar(&cfg.sshKey, "ssh-key", "", "SSH private key")
	flags.IntVar(&cfg.sshPort, "ssh-port", 22, "SSH port")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, writeErr := os.Stderr.Write(flagOutput.Bytes()); writeErr != nil {
				return inputBenchmarkFailure{message: "benchmark help could not be written", cause: writeErr}
			}
			return flag.ErrHelp
		}
		return inputBenchmarkFailure{message: "invalid benchmark flags", cause: err}
	}
	if cfg.runs < 1 || cfg.runs > 20 || cfg.samples < 5 || cfg.samples > 1000 || cfg.startup < time.Second || cfg.timeout <= 0 || cfg.interval < 0 || cfg.sshPort < 1 || cfg.sshPort > 65535 {
		return errors.New("invalid benchmark configuration")
	}

	encoder := json.NewEncoder(output)
	all := make(map[string][]result)
	transports := []string{"ssh", "pb_quic", "pb_wss"}
	for runIndex := 1; runIndex <= cfg.runs; runIndex++ {
		offset := (runIndex - 1) % len(transports)
		order := append(append([]string{}, transports[offset:]...), transports[:offset]...)
		for _, transport := range order {
			value, err := measure(ctx, cfg, transport, runIndex)
			if err != nil {
				return inputBenchmarkFailure{message: fmt.Sprintf("%s run %d failed", transport, runIndex), cause: err}
			}
			if err := encoder.Encode(value); err != nil {
				return inputBenchmarkFailure{message: "benchmark result could not be written", cause: err}
			}
			all[transport] = append(all[transport], value)
		}
	}
	for _, transport := range transports {
		if err := encoder.Encode(summarizeRuns(transport, all[transport])); err != nil {
			return inputBenchmarkFailure{message: "benchmark summary could not be written", cause: err}
		}
	}
	return nil
}

func measure(parent context.Context, cfg config, transport string, runIndex int) (measured result, measureErr error) {
	ctx, cancel := context.WithCancel(parent)
	remote := `exec env TERM=xterm-256color python3 -u -c "import os,tty;tty.setraw(0);[(lambda b:os.write(1,b'PBIN'+b))(os.read(0,1)) for _ in iter(int,1)]"`
	var command *exec.Cmd
	pbSession := ""
	if transport == "ssh" {
		args := []string{"-tt", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-p", fmt.Sprint(cfg.sshPort)}
		if cfg.sshKey != "" {
			args = append(args, "-i", cfg.sshKey)
		}
		args = append(args, cfg.sshHost, remote)
		command = exec.CommandContext(ctx, "ssh", args...)
	} else {
		pbSession = fmt.Sprintf("input-bench-%s-%d-%d", transport[3:], time.Now().UnixNano(), runIndex)
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
	processFinished := false
	var stopReader chan struct{}
	var readerResult *chunkReadResult
	var matcher *streamMatcher
	defer func() {
		cancel()
		var cleanupErrors []error
		if stopReader != nil {
			close(stopReader)
		}
		if terminal != nil {
			if err := terminal.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		if processDone != nil && !processFinished {
			select {
			case waitErr := <-processDone:
				processFinished = true
				if !processTerminatedBy(waitErr, command.ProcessState, syscall.SIGKILL, contextKillSucceeded.Load()) {
					cleanupErrors = append(cleanupErrors, waitErr)
				}
			case <-time.After(2 * time.Second):
				killErr := command.Process.Kill()
				if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					cleanupErrors = append(cleanupErrors, killErr)
				}
				select {
				case waitErr := <-processDone:
					processFinished = true
					if !processTerminatedBy(waitErr, command.ProcessState, syscall.SIGKILL, killErr == nil || contextKillSucceeded.Load()) {
						cleanupErrors = append(cleanupErrors, waitErr)
					}
				case <-time.After(2 * time.Second):
					cleanupErrors = append(cleanupErrors, context.DeadlineExceeded)
				}
			}
		}
		if readerResult != nil {
			select {
			case <-readerResult.done:
				expectedPTYClose := processFinished && (errors.Is(readerResult.err, io.EOF) || errors.Is(readerResult.err, syscall.EIO))
				if readerResult.err != nil && !expectedPTYClose && (matcher == nil || !matcher.readErrReported) {
					cleanupErrors = append(cleanupErrors, readerResult.err)
				}
			case <-time.After(2 * time.Second):
				cleanupErrors = append(cleanupErrors, context.DeadlineExceeded)
			}
		}
		if pbSession != "" {
			if err := deletePBSession(cfg.pb, cfg.target, pbSession); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		if len(cleanupErrors) > 0 {
			message := "benchmark cleanup failed"
			var primary inputBenchmarkFailure
			if errors.As(measureErr, &primary) {
				message = primary.message + "; benchmark cleanup failed"
			}
			measureErr = inputBenchmarkFailure{message: message, cause: errors.Join(measureErr, errors.Join(cleanupErrors...))}
		}
	}()

	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return result{}, inputBenchmarkFailure{message: "terminal process could not be started", cause: err}
	}
	processDone = make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	chunks := make(chan []byte, 256)
	stopReader = make(chan struct{})
	readerResult = &chunkReadResult{done: make(chan struct{})}
	go readChunks(terminal, chunks, stopReader, readerResult)
	matcher = &streamMatcher{chunks: chunks, reader: readerResult}
	if pbSession != "" {
		if err := matcher.waitAny(ctx, cfg.startup, []byte("# "), []byte("$ ")); err != nil {
			return result{}, inputBenchmarkFailure{message: "remote shell did not become ready", cause: err}
		}
		if _, err := io.WriteString(terminal, remote+"\n"); err != nil {
			return result{}, inputBenchmarkFailure{message: "remote command could not be sent", cause: err}
		}
	}
	if err := waitResponder(ctx, terminal, matcher, cfg.startup); err != nil {
		return result{}, inputBenchmarkFailure{message: "raw responder did not become ready", cause: err}
	}

	values := make([]time.Duration, 0, cfg.samples)
	for index := 0; index < cfg.samples; index++ {
		key := byte('a' + index%26)
		started := time.Now()
		if _, err := terminal.Write([]byte{key}); err != nil {
			return result{}, inputBenchmarkFailure{message: "terminal input could not be sent", cause: err}
		}
		if err := matcher.wait(ctx, []byte{'P', 'B', 'I', 'N', key}, cfg.timeout); err != nil {
			return result{}, inputBenchmarkFailure{message: "terminal input response failed", cause: err}
		}
		values = append(values, time.Since(started))
		if err := sleep(ctx, cfg.interval); err != nil {
			return result{}, inputBenchmarkFailure{message: "benchmark was interrupted", cause: err}
		}
	}
	if _, err := terminal.Write([]byte{3}); err != nil {
		return result{}, inputBenchmarkFailure{message: "terminal session could not be stopped", cause: err}
	}
	select {
	case processErr := <-processDone:
		processFinished = true
		processDone = nil
		if processErr != nil && !expectedControlCExit(processErr, command.ProcessState) {
			return result{}, inputBenchmarkFailure{message: "terminal process exited unsuccessfully", cause: processErr}
		}
	case <-time.After(2 * time.Second):
		cancel()
		select {
		case processErr := <-processDone:
			processFinished = true
			processDone = nil
			if processErr != nil && !processTerminatedBy(processErr, command.ProcessState, syscall.SIGKILL, contextKillSucceeded.Load()) {
				return result{}, inputBenchmarkFailure{message: "terminal process exited unsuccessfully", cause: processErr}
			}
		case <-time.After(2 * time.Second):
			return result{}, inputBenchmarkFailure{message: "terminal process did not stop", cause: context.DeadlineExceeded}
		}
	}
	return summarize(transport, runIndex, values), nil
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

func (m *streamMatcher) waitAny(ctx context.Context, timeout time.Duration, markers ...[]byte) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		for _, marker := range markers {
			if index := bytes.Index(m.pending, marker); index >= 0 {
				m.pending = m.pending[index+len(marker):]
				return nil
			}
		}
		select {
		case chunk, ok := <-m.chunks:
			if !ok {
				if cause := context.Cause(ctx); cause != nil {
					return cause
				}
				return m.closedStreamError()
			}
			m.pending = append(m.pending, chunk...)
			m.trimPending()
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-timer.C:
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return errResponseTimeout
		}
	}
}

func waitResponder(ctx context.Context, terminal io.Writer, matcher *streamMatcher, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if _, err := terminal.Write([]byte{'W'}); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		attempt := 500 * time.Millisecond
		if remaining < attempt {
			attempt = remaining
		}
		if err := matcher.wait(ctx, []byte("PBINW"), attempt); err == nil {
			return nil
		} else if errors.Is(err, io.EOF) || !errors.Is(err, errResponseTimeout) {
			return err
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return errResponseTimeout
}

type streamMatcher struct {
	chunks          <-chan []byte
	reader          *chunkReadResult
	pending         []byte
	readErrReported bool
}

func (m *streamMatcher) wait(ctx context.Context, marker []byte, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if index := bytes.Index(m.pending, marker); index >= 0 {
			m.pending = m.pending[index+len(marker):]
			return nil
		}
		m.trimPending()
		select {
		case chunk, ok := <-m.chunks:
			if !ok {
				if cause := context.Cause(ctx); cause != nil {
					return cause
				}
				return m.closedStreamError()
			}
			m.pending = append(m.pending, chunk...)
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-timer.C:
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return errResponseTimeout
		}
	}
}

func (m *streamMatcher) trimPending() {
	if len(m.pending) > 4096 {
		m.pending = append([]byte(nil), m.pending[len(m.pending)-4096:]...)
	}
}

func (m *streamMatcher) closedStreamError() error {
	if m.reader != nil {
		<-m.reader.done
		if m.reader.err != nil {
			m.readErrReported = true
			return m.reader.err
		}
	}
	return io.EOF
}

type chunkReadResult struct {
	done chan struct{}
	err  error
}

func readChunks(reader io.Reader, output chan<- []byte, stop <-chan struct{}, result *chunkReadResult) {
	var readErr error
	defer func() {
		result.err = readErr
		close(result.done)
		close(output)
	}()
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			select {
			case output <- append([]byte(nil), buffer[:n]...):
			case <-stop:
				return
			}
		}
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			readErr = err
			return
		}
	}
}

func processTerminatedBy(waitErr error, state *os.ProcessState, signal syscall.Signal, requestSucceeded bool) bool {
	if waitErr == nil || !requestSucceeded || state == nil {
		return false
	}
	if _, ok := waitErr.(*exec.ExitError); !ok {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal
}

func expectedControlCExit(waitErr error, state *os.ProcessState) bool {
	if waitErr == nil || state == nil {
		return false
	}
	if _, ok := waitErr.(*exec.ExitError); !ok {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled() && status.Signal() == syscall.SIGINT || status.Exited() && status.ExitStatus() == 130
}

func summarize(transport string, runIndex int, values []time.Duration) result {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	value := result{Transport: transport, Run: runIndex, Samples: len(values)}
	value.P50MS = milliseconds(percentile(values, 50))
	value.P95MS = milliseconds(percentile(values, 95))
	value.P99MS = milliseconds(percentile(values, 99))
	value.MaxMS = milliseconds(values[len(values)-1])
	for _, duration := range values {
		if duration > 200*time.Millisecond {
			value.Over200MS++
		}
		if duration > 500*time.Millisecond {
			value.Over500MS++
		}
	}
	return value
}

func summarizeRuns(transport string, values []result) aggregate {
	summary := aggregate{Summary: true, Transport: transport, Runs: len(values)}
	p50, p95, p99, maximums := []float64{}, []float64{}, []float64{}, []float64{}
	for _, value := range values {
		summary.Samples += value.Samples
		summary.TotalOver200MS += value.Over200MS
		summary.TotalOver500MS += value.Over500MS
		p50, p95, p99, maximums = append(p50, value.P50MS), append(p95, value.P95MS), append(p99, value.P99MS), append(maximums, value.MaxMS)
		if value.MaxMS > summary.WorstMS {
			summary.WorstMS = value.MaxMS
		}
	}
	summary.MedianP50MS, summary.MedianP95MS = median(p50), median(p95)
	summary.MedianP99MS, summary.MedianMaxMS = median(p99), median(maximums)
	return summary
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
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 {
		return inputBenchmarkFailure{message: "temporary session cleanup could not be prepared", cause: err}
	}
	var confirmation struct {
		Data struct {
			Token string `json:"confirmation_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(preview, &confirmation); err != nil {
		return inputBenchmarkFailure{message: "temporary session cleanup confirmation was invalid", cause: err}
	}
	if confirmation.Data.Token == "" {
		return inputBenchmarkFailure{message: "temporary session cleanup confirmation was missing", cause: errors.New("confirmation token missing")}
	}
	if err := exec.CommandContext(ctx, pb, "session", "delete", target, session, "--confirm", confirmation.Data.Token, "--json").Run(); err != nil {
		return inputBenchmarkFailure{message: "temporary session cleanup failed", cause: err}
	}
	return nil
}

func closePBSession(ctx context.Context, pb, target, session string) error {
	args := []string{"session", "close", target, session, "--json"}
	output, err := exec.CommandContext(ctx, pb, args...).Output()
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 {
			return inputBenchmarkFailure{message: "temporary session cleanup could not be prepared", cause: err}
		}
		var confirmation struct {
			Data struct {
				Token string `json:"confirmation_token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(output, &confirmation); err != nil {
			return inputBenchmarkFailure{message: "temporary session close confirmation was invalid", cause: err}
		}
		if confirmation.Data.Token == "" {
			return inputBenchmarkFailure{message: "temporary session close confirmation was missing", cause: errors.New("confirmation token missing")}
		}
		args = append(args, "--confirm", confirmation.Data.Token)
		output, err = exec.CommandContext(ctx, pb, args...).Output()
		if err != nil {
			return inputBenchmarkFailure{message: "temporary session close failed", cause: err}
		}
	}
	var result struct {
		State          string `json:"state"`
		OperationState string `json:"operation_state"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return inputBenchmarkFailure{message: "temporary session close result was invalid", cause: err}
	}
	if result.State != "closed" || result.OperationState != "applied" {
		return inputBenchmarkFailure{message: "temporary session close is pending; record left intact", cause: errors.New("close pending")}
	}
	return nil
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func percentile(values []time.Duration, percent int) time.Duration {
	index := (len(values)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	return values[index-1]
}

func median(values []float64) float64 {
	sort.Float64s(values)
	if len(values) == 0 {
		return 0
	}
	return values[(len(values)-1)/2]
}

func milliseconds(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }
