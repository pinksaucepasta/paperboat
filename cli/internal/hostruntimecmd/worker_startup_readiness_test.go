//go:build darwin || linux || windows

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	health, shutdown atomic.Int32
}

func (f *startupReadinessFeature) Health(context.Context) error   { f.health.Add(1); return f.err }
func (f *startupReadinessFeature) Shutdown(context.Context) error { f.shutdown.Add(1); return nil }

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
