package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
)

// stallingSnapshotClient blocks its first request until that request's context ends
// (a daemon that accepts the pipe but never answers), then answers.
type stallingSnapshotClient struct{ calls atomic.Int32 }

func (c *stallingSnapshotClient) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	if c.calls.Add(1) == 1 {
		<-ctx.Done()
		return localapi.Snapshot{}, ctx.Err()
	}
	return localapi.Snapshot{}, nil
}

func TestWaitForLocalDaemonSnapshotRetriesAStalledRequest(t *testing.T) {
	previous := localDaemonSnapshotRequestTimeout
	localDaemonSnapshotRequestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { localDaemonSnapshotRequestTimeout = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &stallingSnapshotClient{}
	started := time.Now()
	if _, err := waitForLocalDaemonSnapshot(ctx, client, "/run/pb/daemon.sock", nil); err != nil {
		t.Fatalf("waitForLocalDaemonSnapshot: %v", err)
	}
	if client.calls.Load() < 2 {
		t.Fatalf("calls = %d, want a retry after the stalled request", client.calls.Load())
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("one stalled request held the wait for %s", elapsed)
	}
}

func TestWaitForLocalDaemonSnapshotStalledUntilDeadlineSaysSo(t *testing.T) {
	previous := localDaemonSnapshotRequestTimeout
	localDaemonSnapshotRequestTimeout = 30 * time.Millisecond
	t.Cleanup(func() { localDaemonSnapshotRequestTimeout = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := waitForLocalDaemonSnapshot(ctx, neverAnswers{}, "/run/pb/daemon.sock", nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("err = %v, want a did-not-answer error", err)
	}
}

type neverAnswers struct{}

func (neverAnswers) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	<-ctx.Done()
	return localapi.Snapshot{}, ctx.Err()
}

func TestCountServiceStartTimeouts(t *testing.T) {
	now := time.Now().UTC()
	var lines []string
	for _, event := range []diagnostics.Event{
		{Schema: diagnostics.EventSchemaV1, At: now.Add(-48 * time.Hour), Category: "local_daemon", Code: localdaemon.ServiceStartTimeoutCode, Severity: "warning"},
		{Schema: diagnostics.EventSchemaV1, At: now.Add(-time.Hour), Category: "local_daemon", Code: localdaemon.ServiceStartTimeoutCode, Severity: "warning"},
		{Schema: diagnostics.EventSchemaV1, At: now.Add(-time.Minute), Category: "local_daemon", Code: "other", Severity: "info"},
	} {
		line, _ := json.Marshal(event)
		lines = append(lines, string(line))
	}
	count, last := countServiceStartTimeouts([]byte(strings.Join(lines, "\n")), now.Add(-serviceStartTimeoutWindow))
	if count != 1 || !last.Equal(now.Add(-time.Hour)) {
		t.Fatalf("count=%d last=%s", count, last)
	}
}
