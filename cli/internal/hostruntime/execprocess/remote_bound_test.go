package execprocess

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

// Calls that carry no caller context do not hang on a hostd that stopped answering (audit C #30).
func TestRemoteCallsWithoutACallerContextAreBounded(t *testing.T) {
	previous := remoteCallBound
	remoteCallBound = 50 * time.Millisecond
	t.Cleanup(func() { remoteCallBound = previous })
	manager, err := NewRemoteManager(func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := manager.Get("operation_1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get on a silent hostd: %v", err)
	}
	execution := &remoteExecution{manager: manager, id: "operation_1"}
	if err := execution.Signal(pty.Terminate); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Signal on a silent hostd: %v", err)
	}
	if _, err := execution.OpenReader(1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenReader on a silent hostd: %v", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("bounded calls took %v", took)
	}
	// A caller's own context still decides a waiting call.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := execution.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with the caller's context: %v", err)
	}
}
