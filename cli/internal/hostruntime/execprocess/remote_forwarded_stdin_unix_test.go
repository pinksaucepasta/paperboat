//go:build darwin || linux

package execprocess

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

// `echo hi | pb exec <m> -- cat` printed nothing on every platform after
// commands moved into session owners (407955aa): hostd serves a forwarded
// execution, and RemoteServer's write refused it (no WriteContext), so every
// stdin chunk was dropped. Real process, both hops: stdin bytes in must equal
// output bytes out, through EOF, for small and large input.
func TestForwardedExecutionDeliversStdinThroughBothHops(t *testing.T) {
	root := t.TempDir()
	owner, err := New(Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 4, MaximumOperations: 16, ReplayBytes: 4 << 20, ChunkBytes: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = owner.Shutdown(ctx)
	})
	inner := &RemoteServer{Manager: owner, MaximumReaders: 8}
	forwarded, err := NewRemoteManager(inner.Call) // the session owner, as hostd's router reaches it
	if err != nil {
		t.Fatal(err)
	}
	hostd := &RemoteServer{Manager: forwarded, MaximumReaders: 8}
	client, err := NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return hostd.Call(ctx, op, body) // the worker, as it reaches hostd
	})
	if err != nil {
		t.Fatal(err)
	}
	large := make([]byte, 1<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string][]byte{"small": []byte("hi\n"), "chunked": bytes.Repeat([]byte("0123456789"), 10_000), "large": large} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			execution, _, err := client.Start(ctx, Request{OperationID: "operation_stdin_" + name, Argv: []string{"/bin/cat"}, CWD: root})
			if err != nil {
				t.Fatal(err)
			}
			// The exec stream answers start only after WaitStarted (server/dispatcher_exec.go).
			if _, err := execution.WaitStarted(ctx); err != nil {
				t.Fatal(err)
			}
			for rest := input; len(rest) > 0; {
				n := min(len(rest), 48<<10)
				written, err := execution.Write(rest[:n])
				if err != nil || written != n {
					t.Fatalf("stdin write: wrote %d of %d: %v", written, n, err)
				}
				rest = rest[n:]
			}
			if err := execution.CloseInput(); err != nil {
				t.Fatal(err)
			}
			var output []byte
			for from := uint64(1); ; {
				event, err := execution.Next(ctx, from)
				if err != nil {
					t.Fatal(err)
				}
				from = event.Sequence + 1
				if event.Stream == "stdout" {
					output = append(output, event.Data...)
				}
				if event.Result != nil {
					if event.Result.Code != 0 {
						t.Fatalf("cat exit %d", event.Result.Code)
					}
					break
				}
			}
			if !bytes.Equal(output, input) {
				t.Fatalf("stdin %d bytes in, %d bytes out", len(input), len(output))
			}
		})
	}
}
