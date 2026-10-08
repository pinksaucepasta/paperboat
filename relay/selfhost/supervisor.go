package selfhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
			capability := filepath.Base(componentDir)
			binary := filepath.Join(binaryDir, "paperboat-"+capability)
			info, err := os.Stat(binary)
			if err != nil {
				return fmt.Errorf("runtime binary unavailable: %w", err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return fmt.Errorf("%s runtime binary unavailable", capability)
			}
		}
		for _, componentDir := range dirs {
			wg.Add(1)
			go func(componentDir string) {
				defer wg.Done()
				supervise(childContext, filepath.Join(binaryDir, "paperboat-"+filepath.Base(componentDir)), componentDir, output, observe)
			}(componentDir)
		}
		started = true
		return nil
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
func supervise(ctx context.Context, binary, dir string, output io.Writer, observe func(context.Context, string, error)) {
	delay := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, binary, "run", "--state-dir", dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout = output
		cmd.Stderr = output
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = 6 * time.Second
		started := time.Now()
		err := cmd.Run()
		// The group belongs to this runtime alone. Retire descendants even if
		// the parent crashed or exited before cancellation finished.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(output, "%s stopped; restarting in %s\n", strings.TrimPrefix(filepath.Base(binary), "paperboat-"), delay)
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
