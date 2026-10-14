//go:build darwin || linux || windows

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// Exercise the actual authenticated lifecycle listener and worker entrypoint.
// A one-minute interval ensures readiness cannot borrow a later periodic tick.
func TestWorkerStartupRequiresHealthyFeatureAndInitialHeartbeat(t *testing.T) {
	for _, mode := range []string{"healthy", "unhealthy", "fenced"} {
		t.Run(mode, func(t *testing.T) {
			server, endpoint, token, args := workerStartupFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, err := hostdproto.NewClient(endpoint, token, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, err = client.Active(ctx); err == nil {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("startup listener unavailable", err)
				}
				time.Sleep(time.Millisecond)
			}
			feature := &startupReadinessFeature{}
			unhealthy := errors.New("fixture feature is unhealthy")
			if mode == "unhealthy" {
				feature.err = unhealthy
			}
			output := &startupReadinessOutput{server: server, acknowledged: make(chan struct{}, 1)}
			done := make(chan error, 1)
			workerCtx, stopWorker := context.WithCancel(ctx)
			defer stopWorker()
			args = append(args, "--worker-id", "startup-worker", "--version", "test", "--heartbeat", "1m")
			go func() {
				done <- runWorkerWith(workerCtx, args, strings.NewReader(""), output, &bytes.Buffer{}, func(ctx context.Context, _ string, _ []byte, _ string, _ uint64, _ string) (workerFeature, error) {
					if mode == "fenced" {
						rival, err := hostdproto.NewCandidate(client, "replacement-worker", "test", 1, 1)
						if err != nil {
							return nil, err
						}
						if _, err = rival.Ready(ctx); err != nil {
							return nil, err
						}
						if _, err = rival.Activate(ctx); err != nil {
							return nil, err
						}
					}
					return feature, nil
				})
			}()
			if mode == "healthy" {
				select {
				case <-output.acknowledged:
					if err := output.readinessError(); err != nil {
						t.Error(err)
					}
				case err := <-done:
					t.Fatalf("worker exited before readiness: %v", err)
				case <-ctx.Done():
					t.Fatal("worker did not acknowledge readiness")
				}
				stopWorker()
			}
			select {
			case err := <-done:
				if mode == "healthy" && err != nil {
					t.Fatal(err)
				}
				if mode == "unhealthy" && !errors.Is(err, unhealthy) {
					t.Fatalf("health failure=%v", err)
				}
				if mode == "fenced" && !errors.Is(err, hostdproto.ErrFenced) {
					t.Fatalf("heartbeat fencing failure=%v", err)
				}
			case <-ctx.Done():
				t.Fatal("worker did not exit")
			}
			if mode != "healthy" {
				select {
				case <-output.acknowledged:
					t.Fatal("failed startup acknowledged active readiness")
				default:
				}
			}
			if feature.health.Load() != 1 || feature.shutdown.Load() != 1 {
				t.Fatalf("health=%d shutdown=%d", feature.health.Load(), feature.shutdown.Load())
			}
		})
	}
}

type startupReadinessFeature struct {
	err              error
	shutdownErr      error
	health, shutdown atomic.Int32
}

func (f *startupReadinessFeature) Health(context.Context) error { f.health.Add(1); return f.err }
func (f *startupReadinessFeature) Shutdown(context.Context) error {
	f.shutdown.Add(1)
	return f.shutdownErr
}

func TestWorkerStartupRetainsCleanupFailureAndAllowsFreshRecovery(t *testing.T) {
	_, endpoint, token, args := workerStartupFixture(t)
	client, err := hostdproto.NewClient(endpoint, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer startupCancel()
	for {
		if _, err := client.Active(startupCtx); err == nil {
			break
		}
		if startupCtx.Err() != nil {
			t.Fatal("fixture listener did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	args = append(args, "--worker-id", "cleanup-worker", "--version", "test", "--heartbeat", "1m")
	primary := errors.New("private startup detail")
	cleanup := errors.New("private cleanup detail")
	feature := &startupReadinessFeature{err: primary, shutdownErr: cleanup}
	err = runWorkerWith(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, string, []byte, string, uint64, string) (workerFeature, error) {
		return feature, nil
	})
	if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
		t.Fatalf("startup or cleanup cause was discarded: %T", err)
	}
	if feature.shutdown.Load() != 1 {
		t.Fatal("feature cleanup was repeated")
	}
	// A fresh authenticated owner can start after the failed worker.
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	output := &workerRecoveryOutput{ready: ready}
	done := make(chan error, 1)
	go func() {
		done <- runWorkerWith(ctx, args, strings.NewReader(""), output, io.Discard, func(context.Context, string, []byte, string, uint64, string) (workerFeature, error) {
			return &startupReadinessFeature{}, nil
		})
	}()
	select {
	case <-ready:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("fresh worker did not become ready")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type workerRecoveryOutput struct{ ready chan struct{} }

func (o *workerRecoveryOutput) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "active ") {
		close(o.ready)
	}
	return len(p), nil
}

type startupReadinessOutput struct {
	server       *hostdproto.Server
	acknowledged chan struct{}
	mu           sync.Mutex
	err          error
}

func (o *startupReadinessOutput) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "active ") {
		status := o.server.Status()
		o.mu.Lock()
		if status.State != hostdproto.StateActive || status.LastHeartbeatUnixMilli == 0 || time.Since(time.UnixMilli(status.LastHeartbeatUnixMilli)) > time.Second {
			o.err = fmt.Errorf("acknowledged readiness without fresh active heartbeat: %+v", status)
		}
		o.mu.Unlock()
		o.acknowledged <- struct{}{}
	}
	return len(p), nil
}
func (o *startupReadinessOutput) readinessError() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func TestWorkerActivationCancellationRetainsBorrowedPipeAndRecovery(t *testing.T) {
	_, endpoint, token, args := workerStartupFixture(t)
	listenerCtx, stopListener := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopListener()
	client, err := hostdproto.NewClient(endpoint, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = client.Active(listenerCtx); err == nil {
			break
		}
		if listenerCtx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("private cancellation owner")
	output := &activationCancelOutput{cancel: func() { cancel(cause) }}
	args = append(args, "--worker-id", "activation-candidate", "--version", "test", "--wait-activation")
	err = runWorkerWith(ctx, args, reader, output, io.Discard, func(context.Context, string, []byte, string, uint64, string) (workerFeature, error) {
		t.Fatal("canceled activation started the feature")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("activation lost cancellation ownership: %T", err)
	}
	sent := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, "activate\n"); sent <- err }()
	recoveryCtx, stopRecovery := context.WithTimeout(context.Background(), time.Second)
	defer stopRecovery()
	if err := awaitWorkerActivation(recoveryCtx, reader); err != nil {
		t.Fatalf("borrowed pipe did not recover: %T", err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

type activationCancelOutput struct{ cancel func() }

func (w *activationCancelOutput) Write(value []byte) (int, error) {
	if strings.HasPrefix(string(value), "ready ") {
		w.cancel()
	}
	return len(value), nil
}
