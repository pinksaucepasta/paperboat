package httpapi

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type fakeRunningTerminals struct {
	sessions []usermachines.SharedTerminalSession
}

func (f fakeRunningTerminals) TerminalSessionInventory(_ context.Context, _ string, opts usermachines.InventoryOptions) ([]usermachines.SharedTerminalSession, int, error) {
	if opts.State != "running" || !opts.WithoutLiveMetadata {
		return nil, 0, fmt.Errorf("unexpected terminal query %+v", opts)
	}
	end := min(opts.Offset+opts.Limit, len(f.sessions))
	return f.sessions[opts.Offset:end], len(f.sessions), nil
}

// fakeCursorList serves n items in pages of the requested size; it fails unless the state filter is active.
func pageOf(ctx context.Context, cursor string, limit, n int) (from, to int, next string, err error) {
	if filters := workspace.InventoryFiltersFromContext(ctx); filters.State != "active" {
		return 0, 0, "", fmt.Errorf("list without the active filter: %+v", filters)
	}
	if cursor != "" {
		if _, err := fmt.Sscan(cursor, &from); err != nil {
			return 0, 0, "", err
		}
	}
	to = min(from+limit, n)
	if to < n {
		next = fmt.Sprint(to)
	}
	return from, to, next, nil
}

type fakeRunningTunnels struct {
	tunnelv1.API
	n int
}

func (f fakeRunningTunnels) ListTunnels(ctx context.Context, _ previewtunnelapi.RequestContext, cursor string, limit int) (tunnelv1.TunnelPage, error) {
	from, to, next, err := pageOf(ctx, cursor, limit, f.n)
	page := tunnelv1.TunnelPage{NextCursor: next}
	for i := from; i < to; i++ {
		page.Items = append(page.Items, tunnelv1.TunnelView{CreatedByHostID: fmt.Sprintf("machine_%d", i%3)})
	}
	return page, err
}

type fakeRunningPreviews struct {
	PreviewLeaseAPI
	n int
}

func (f fakeRunningPreviews) List(ctx context.Context, _ previewtunnelapi.RequestContext, cursor string, limit int) (previewv1.PreviewPage, error) {
	from, to, next, err := pageOf(ctx, cursor, limit, f.n)
	page := previewv1.PreviewPage{NextCursor: next}
	for i := from; i < to; i++ {
		page.Items = append(page.Items, previewv1.Preview{OwnerMachineID: fmt.Sprintf("machine_%d", i%2)})
	}
	return page, err
}

// The console paged up to 2,000 previews and 2,000 tunnels every 30 s per tab and truncated above that. The counts
// group the collections' own list pages by machine, over every page.
func TestRunningCountsGroupEveryPageByMachine(t *testing.T) {
	terminals := fakeRunningTerminals{}
	for i := 0; i < 450; i++ {
		terminals.sessions = append(terminals.sessions, usermachines.SharedTerminalSession{Target: usermachines.SharedTerminalTarget{MachineID: "machine_0"}})
	}
	ctx := workspace.WithContext(context.Background(), workspace.Scope{ID: "personal"})
	counts, err := countRunning(ctx, terminals, fakeRunningPreviews{n: 2501}, fakeRunningTunnels{n: 2999}, "user_1", previewtunnelapi.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	want := []machineRunning{{"machine_0", 450, 1000, 1251}, {"machine_1", 0, 1000, 1250}, {"machine_2", 0, 999, 0}}
	if counts.Truncated || fmt.Sprint(counts.Items) != fmt.Sprint(want) {
		t.Fatalf("counts %+v, want %+v (not truncated)", counts, want)
	}
	counts, err = countRunning(ctx, terminals, fakeRunningPreviews{n: runningCountsMaxRows + 500}, nil, "user_1", previewtunnelapi.RequestContext{})
	if err != nil || !counts.Truncated {
		t.Fatalf("past %d previews: truncated=%v err=%v, want truncated", runningCountsMaxRows, counts.Truncated, err)
	}
}

// Every console tab asks every 30 s; one computation per key serves them all for the TTL, and a failure is not kept.
func TestRunningCountsCacheSharesOneComputationPerKey(t *testing.T) {
	var cache runningCountsCache
	var computed atomic.Int32
	release := make(chan struct{})
	compute := func() (runningCounts, error) {
		computed.Add(1)
		<-release
		return runningCounts{Items: []machineRunning{{MachineID: "machine_0", Terminals: 1}}}, nil
	}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if counts, err := cache.get(context.Background(), "user_1\x00personal", now, compute); err != nil || len(counts.Items) != 1 {
				t.Errorf("shared answer %+v %v", counts, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := computed.Load(); n != 1 {
		t.Fatalf("20 concurrent tabs ran %d computations, want 1", n)
	}
	if _, err := cache.get(context.Background(), "user_1\x00personal", now.Add(runningCountsTTL/2), compute); err != nil || computed.Load() != 1 {
		t.Fatalf("a fresh entry was recomputed (%d)", computed.Load())
	}
	if _, err := cache.get(context.Background(), "user_1\x00personal", now.Add(2*runningCountsTTL), compute); err != nil || computed.Load() != 2 {
		t.Fatalf("a stale entry was not recomputed (%d)", computed.Load())
	}
	failing := func() (runningCounts, error) {
		computed.Add(1)
		return runningCounts{}, fmt.Errorf("database unavailable")
	}
	_, _ = cache.get(context.Background(), "user_2\x00personal", now, failing)
	if _, err := cache.get(context.Background(), "user_2\x00personal", now, compute); err != nil {
		t.Fatalf("a failure was kept: %v", err)
	}
}
