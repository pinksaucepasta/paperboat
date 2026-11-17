//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
)

// The real launcher still invokes the fixed daemon entry point. This isolated
// child consumes the production capability FD and worker-control protocol;
// feature behavior is exercised by its separate runtime owner tests.
func init() {
	if os.Getenv("PAPERBOAT_NATIVE_STARTUP_TEST_CHILD") != "1" || len(os.Args) < 3 || os.Args[1] != "daemon" || os.Args[2] != "__runtime-worker" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := runWorkerWith(ctx, os.Args[3:], os.Stdin, os.Stdout, io.Discard, func(context.Context, string, []byte, string, uint64, string) (workerFeature, error) {
		return testWorkerFeature{}, nil
	})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeWorkerProcessUsesSealedCapabilityAndOwnerLifetime(t *testing.T) {
	for _, mode := range []string{"activate and stop", "owner cancellation", "wrong capability"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PAPERBOAT_NATIVE_STARTUP_TEST_CHILD", "1")
			server, endpoint, token, _ := workerStartupFixture(t)
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(endpoint); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("isolated owner listener readiness expired")
				}
				time.Sleep(time.Millisecond)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "wrong capability" {
				token = append([]byte(nil), token...)
				token[0] ^= 1
			}
			ownerCtx, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			process, err := (nativeWorkerStarter{}).Start(ownerCtx, workerStartRequest{Executable: executable, Version: "native-startup", WorkerID: "runtime-native-startup", UID: os.Geteuid(), GID: os.Getegid(), APIMin: 1, APIMax: 1, HostdEndpoint: endpoint, Capability: token, MutationsDisabled: true})
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if err := process.Stop(ctx); err != nil {
						t.Error("worker cleanup", err)
					}
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ready, err := process.Ready(ctx)
			if mode == "wrong capability" {
				if err == nil {
					t.Fatal("foreign capability admitted native worker")
				}
			} else {
				if err != nil || ready.State != hostdproto.StateCandidate || ready.WorkerID != "runtime-native-startup" || ready.Epoch == 0 || ready.APIVersion != 1 {
					t.Fatal("sealed worker did not become ready", err)
				}
				if server.Status().State != hostdproto.StateCandidate {
					t.Fatal("worker mutated owner before private activation")
				}
				active, err := process.Activate(ctx)
				if err != nil || active.State != hostdproto.StateActive || active.Epoch != ready.Epoch || server.Status().State != hostdproto.StateActive {
					t.Fatal("worker activation lost fenced identity", err)
				}
			}
			if mode == "owner cancellation" {
				cancelOwner()
			}
			if err := process.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			stopped = true
			native := process.(*execWorker)
			if native.command.ProcessState == nil || (!native.command.ProcessState.Exited() && !native.command.ProcessState.Sys().(syscall.WaitStatus).Signaled()) {
				t.Fatal("native owner left a live child or unreaped process")
			}
		})
	}
}

// TestDumpActiveStacksCapturesAFrozenWorker: a worker that stopped heartbeating
// is aborted by hostd so the daemon log names where its goroutines were stuck.
func TestDumpActiveStacksCapturesAFrozenWorker(t *testing.T) {
	t.Setenv("PAPERBOAT_NATIVE_STARTUP_TEST_CHILD", "1")
	_, endpoint, token, _ := workerStartupFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := (nativeWorkerStarter{}).Start(t.Context(), workerStartRequest{Executable: executable, Version: "stack-dump", WorkerID: "runtime-stack-dump", UID: os.Geteuid(), GID: os.Getegid(), APIMin: 1, APIMax: 1, HostdEndpoint: endpoint, Capability: token, MutationsDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := process.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	owned := &ownedWorkers{active: "w", workers: map[string]*ownedWorker{"w": {process: process}}}
	var sink strings.Builder
	owned.dumpActiveStacks(ctx, &sink)
	if out := sink.String(); !strings.Contains(out, "goroutine ") || !strings.Contains(out, "runWorkerWith") {
		t.Fatalf("no goroutine stacks of the worker in the dump:\n%.2000s", out)
	}
	select {
	case <-process.(*execWorker).Exited():
	default:
		t.Fatal("the dumped worker is still running")
	}
}
