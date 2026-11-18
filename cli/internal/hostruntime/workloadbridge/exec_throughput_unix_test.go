//go:build darwin || linux

package workloadbridge

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

// bridgeTo serves executions on a bridge socket under root and returns a client for it.
func bridgeTo(t testing.TB, ctx context.Context, root, name string, executions execprocess.Service) *Client {
	t.Helper()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }, TerminationGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Shutdown(context.Background()) })
	token := make([]byte, 32)
	token[0] = 9
	var mu sync.Mutex
	fence := hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1, APIVersion: 1}
	server := &Server{Endpoint: filepath.Join(root, name, "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 4}, MaximumConcurrent: 8, Ready: make(chan error, 1), Fence: func() hostdproto.Status { mu.Lock(); defer mu.Unlock(); return fence }, AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil }}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.Endpoint, token, "worker", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

// BenchmarkExecOutputThroughTwoBridgeHops is the worker -> hostd -> session owner relay of `pb exec` output with no
// QUIC and no network: one reader, one piggybacked ack per event, 128 KiB events, exactly as the worker drains them.
func BenchmarkExecOutputThroughTwoBridgeHops(b *testing.B) {
	root := shorttemp.Dir(b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 8, ChunkBytes: 128 << 10})
	if err != nil {
		b.Fatal(err)
	}
	defer owner.Shutdown(context.Background())
	ownerClient := bridgeTo(b, ctx, root, "owner", owner)
	hostdExecutions, err := ownerClient.Executions()
	if err != nil {
		b.Fatal(err)
	}
	workerClient := bridgeTo(b, ctx, root, "hostd", hostdExecutions)
	worker, err := workerClient.Executions()
	if err != nil {
		b.Fatal(err)
	}
	const total = 256 << 20
	b.SetBytes(total)
	for i := 0; i < b.N; i++ {
		id := "operation_bench_" + strconv.Itoa(i)
		execution, _, err := worker.Start(ctx, execprocess.Request{OperationID: id, Argv: []string{"/bin/dd", "if=/dev/zero", "bs=1M", "count=256"}, CWD: root})
		if err != nil {
			b.Fatal(err)
		}
		reader, err := execution.OpenReader(1)
		if err != nil {
			b.Fatal(err)
		}
		got := 0
		for {
			event, release, err := reader.Next(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if event.Stream == "stdout" {
				got += len(event.Data)
			}
			done := event.Result != nil
			release()
			if done {
				break
			}
		}
		if got != total {
			b.Fatalf("read %d bytes, want %d", got, total)
		}
		_ = reader.Close()
	}
}

// An exec answer is a raw frame, an exec failure keeps the JSON error envelope, and a body that is not a frame
// (the old JSON shape) is refused: nothing falls back to JSON.
func TestExecBridgeAnswersFramesAndRefusesJSONBodies(t *testing.T) {
	root := shorttemp.Dir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Shutdown(context.Background())
	client := bridgeTo(t, ctx, root, "owner", owner)
	listFrame, _ := execprocess.EncodeMessage(struct{}{}, nil)
	result, err := client.Call(ctx, "exec", "list", listFrame)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots struct{ Snapshots []execprocess.Snapshot }
	if _, err := execprocess.DecodeMessage(result, &snapshots); err != nil {
		t.Fatalf("list answer is not a frame: %v", err)
	}
	if _, err := client.Call(ctx, "exec", "list", []byte(`{}`)); !errors.Is(err, execprocess.ErrInvalid) {
		t.Fatalf("JSON body: %v, want ErrInvalid", err)
	}
	getFrame, _ := execprocess.EncodeMessage(map[string]string{"OperationID": "operation_missing"}, nil)
	if _, err := client.Call(ctx, "exec", "get", getFrame); !errors.Is(err, execprocess.ErrNotFound) {
		t.Fatalf("unknown operation: %v, want ErrNotFound through the JSON error envelope", err)
	}
}
