package selfhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"
)

// Run keeps ownership of runtime subprocesses until cancellation. A runtime
// crash is retried with bounded backoff; terminating pbh drains all children.
func Run(ctx context.Context, dir, binaryDir string, output io.Writer, observe func(context.Context, string, error)) error {
	ticker := time.NewTicker(certificateCheckInterval)
	defer ticker.Stop()
	return run(ctx, dir, binaryDir, output, ticker.C, observe)
}
func run(ctx context.Context, dir, binaryDir string, output io.Writer, renewals <-chan time.Time, observe func(context.Context, string, error)) error {
	return runWithLauncher(ctx, dir, binaryLauncher{directory: binaryDir}, output, renewals, observe)
}

// RuntimeLauncher runs exactly one selected component until it exits or ctx is
// canceled. Implementations own readiness and bounded cleanup of their runtime
// (including containers); they must finish cleanup before Run returns. Generated
// state paths must be mounted at the same absolute paths inside containers.
// The installation remains the sole enrollment and certificate owner.
type RuntimeLauncher interface {
	Validate(context.Context, string, string) error
	Run(context.Context, string, string, io.Writer) error
}

// RunWithLauncher lets pbh use its Docker lifecycle without a second supervisor.
// All components are validated before any is launched; crashes and certificate
// replacement use the same cancellation, drain and restart path.
func RunWithLauncher(ctx context.Context, dir string, launcher RuntimeLauncher, output io.Writer, observe func(context.Context, string, error)) error {
	if launcher == nil {
		return errors.New("runtime launcher is required")
	}
	ticker := time.NewTicker(certificateCheckInterval)
	defer ticker.Stop()
	return runWithLauncher(ctx, dir, launcher, output, ticker.C, observe)
}

func runWithLauncher(ctx context.Context, dir string, launcher RuntimeLauncher, output io.Writer, renewals <-chan time.Time, observe func(context.Context, string, error)) error {
	output = &synchronizedWriter{writer: output}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	if _, err := RenewCertificate(dir, time.Now()); err != nil {
		return fmt.Errorf("prepare installation TLS certificate: %w", err)
	}
	childContext, stopChildren := context.WithCancel(ctx)
	defer stopChildren()
	var wg sync.WaitGroup
	started := false
	start := func() error {
		if started {
			return nil
		}
		ready, err := RuntimeReady(dir)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		dirs, err := ComponentDirectories(dir)
		if err != nil {
			return err
		}
		for _, componentDir := range dirs {
			if err := launcher.Validate(childContext, filepath.Base(componentDir), componentDir); err != nil {
				return fmt.Errorf("prepare %s runtime: %w", filepath.Base(componentDir), err)
			}
		}
		for _, componentDir := range dirs {
			wg.Add(1)
			go func(componentDir string) {
				defer wg.Done()
				supervise(childContext, launcher, filepath.Base(componentDir), componentDir, output, observe)
			}(componentDir)
		}
		started = true
		return nil
	}
	if err := syncPublicHTTPSDeployment(dir); err != nil {
		return err
	}
	if err := start(); err != nil {
		return err
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- Serve(ctx, dir, notify, observe) }()
	for {
		select {
		case err := <-serverDone:
			cancel()
			stopChildren()
			wg.Wait()
			return err
		case <-wake:
			if err := start(); err != nil {
				cancel()
				stopChildren()
				<-serverDone
				wg.Wait()
				return err
			}
		case now := <-renewals:
			renewed, err := RenewCertificate(dir, now)
			if err != nil {
				if observe != nil {
					observe(ctx, "selfhost_certificate", err)
				}
				fmt.Fprintln(output, "Infrastructure certificate renewal unavailable; existing certificate retained; retry scheduled.")
				continue
			}
			if renewed {
				stopChildren()
				wg.Wait()
				childContext, stopChildren = context.WithCancel(ctx)
				started = false
				if err := start(); err != nil {
					cancel()
					stopChildren()
					<-serverDone
					wg.Wait()
					return err
				}
				fmt.Fprintln(output, "Infrastructure certificate renewed; runtime components restarted with the same TLS identity.")
			}
		case <-ctx.Done():
			cancel()
			stopChildren()
			err := <-serverDone
			wg.Wait()
			return err
		}
	}
}
func supervise(ctx context.Context, launcher RuntimeLauncher, capability, dir string, output io.Writer, observe func(context.Context, string, error)) {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := launcher.Run(ctx, capability, dir, output)
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(output, "%s stopped; restarting in %s\n", capability, delay)
		if observe != nil && err != nil {
			observe(ctx, "selfhost_child", err)
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

// Component stdout/stderr may be copied concurrently by os/exec.
type synchronizedWriter struct {
	mutex  sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.writer.Write(p)
}
