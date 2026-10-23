// paperboat-watchdog is scheduled on an independent host. Remote cron monitors
// detect both failed probes and absence of the watchdog itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/jobmonitor"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type target struct {
	Monitor string `json:"monitor"`
	URL     string `json:"url"`
}
type configuration struct {
	Targets []target `json:"targets"`
}

type runError struct {
	message string
	cause   error
}

func (e runError) Error() string { return e.message }
func (e runError) Unwrap() error { return e.cause }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 1 || os.Getenv("PAPERBOAT_SENTRY_ENABLED") != "true" {
		return errors.New("watchdog requires one configuration file and enabled monitoring")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return runError{message: "watchdog configuration unavailable", cause: err}
	}
	decoder := json.NewDecoder(io.LimitReader(f, 65537))
	decoder.DisallowUnknownFields()
	var cfg configuration
	decodeErr := decoder.Decode(&cfg)
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				decodeErr = errors.New("multiple watchdog configuration values")
			} else {
				decodeErr = err
			}
		}
	}
	if decodeErr == nil && (len(cfg.Targets) < 1 || len(cfg.Targets) > 16) {
		decodeErr = errors.New("invalid watchdog target count")
	}
	closeErr := f.Close()
	if closeErr != nil {
		return runError{message: "watchdog configuration unavailable", cause: errors.Join(decodeErr, closeErr)}
	}
	if decodeErr != nil {
		return runError{message: "invalid watchdog configuration", cause: decodeErr}
	}
	monitors := make([]*jobmonitor.Monitor, 0, len(cfg.Targets))
	defer func() {
		for _, m := range monitors {
			m.Close()
		}
	}()
	seen := map[string]bool{}
	for _, t := range cfg.Targets {
		if seen[t.Monitor] {
			return errors.New("duplicate watchdog monitor")
		}
		seen[t.Monitor] = true
		m, err := jobmonitor.New(jobmonitor.Config{Enabled: true, DSN: os.Getenv("PAPERBOAT_SENTRY_DSN"), Release: os.Getenv("PAPERBOAT_SENTRY_RELEASE"), Environment: os.Getenv("PAPERBOAT_SENTRY_ENVIRONMENT"), Slug: t.Monitor})
		if err != nil {
			return runError{message: "watchdog monitoring unavailable", cause: err}
		}
		monitors = append(monitors, m)
	}
	results := make([]bool, len(cfg.Targets))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, t := range cfg.Targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			select {
			case limit <- struct{}{}:
				defer func() { <-limit }()
			case <-ctx.Done():
				return
			}
			finish := monitors[i].Start()
			probeErr := jobmonitor.Probe(ctx, t.URL, 5*time.Second)
			results[i] = probeErr == nil
			observeProbeFailure(ctx, probeErr)
			finish(results[i])
		}(i, t)
	}
	wg.Wait()
	failed := 0
	for _, ok := range results {
		if !ok {
			failed++
		}
	}
	if err := json.NewEncoder(output).Encode(map[string]int{"checked": len(results), "failed": failed}); err != nil {
		return runError{message: "watchdog output unavailable", cause: err}
	}
	if failed > 0 {
		return errors.New("watchdog detected unavailable targets")
	}
	return nil
}

func observeProbeFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}
	reference := supportreport.NewReference()
	ctx = supportreport.WithFailureIdentity(ctx, "", "", reference)
	const component, operation, stage, code = "paperboat-server", "service_watchdog", "service_error", "control_unavailable"
	if supportreport.ProjectFault(ctx, component, operation, stage, code, err).Cause == "context_canceled" {
		return
	}
	supportreport.ObserveFromContext(ctx, component, operation, stage, code, err)
}
