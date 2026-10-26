package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// PLAN 3b File transfer modes: the console shows a send as sending, paused with its reason, sending again,
// then done; reports are batched while sending and never block the transfer.
func TestTransferProgressReportsStatesInOrder(t *testing.T) {
	var mu sync.Mutex
	var reports []api.TransferProgress
	progress := &transferProgress{mode: "unlimited", committed: map[string]int64{}, report: func(_ context.Context, p api.TransferProgress) error {
		mu.Lock()
		reports = append(reports, p)
		mu.Unlock()
		return nil
	}}
	settle := func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			progress.mu.Lock()
			idle := !progress.sending
			progress.mu.Unlock()
			if idle {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	progress.set("sending", "")
	settle()
	progress.Committed("ft_1", 100) // within the interval: not reported
	settle()
	progress.Waiting("Waiting for a direct connection or your relay")
	settle()
	progress.Committed("ft_1", 200) // the first bytes after a pause: sending again at once
	settle()
	progress.finish("done", 300)
	mu.Lock()
	defer mu.Unlock()
	want := []api.TransferProgress{
		{TransferMode: "unlimited", TransferState: "sending"},
		{TransferMode: "unlimited", TransferState: "paused", PauseReason: "no_eligible_path", CommittedBytes: 100},
		{TransferMode: "unlimited", TransferState: "sending", CommittedBytes: 200},
		{TransferMode: "unlimited", TransferState: "done", CommittedBytes: 300},
	}
	if len(reports) != len(want) {
		t.Fatalf("reports=%+v", reports)
	}
	for i := range want {
		if reports[i] != want[i] {
			t.Fatalf("report %d = %+v, want %+v", i, reports[i], want[i])
		}
	}
}
