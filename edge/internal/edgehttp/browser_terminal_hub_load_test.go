//go:build load

package edgehttp

// Load check for the edge's shared browser-terminal fan-out (PLAN 3b scale matrix, Lane W): an account with 100 browser
// terminals on one edge node, one of them shared with 50 viewers plus a viewer that never reads. Reports delivered
// records/s, publish-to-receive latency, and whether the stalled viewer is lagged (backlog dropped) without slowing the others. The
// single-terminal run is the baseline: if 100 terminals deliver far less than 100× a single one per publisher, the hub's
// one lock is the node's ceiling.
//
//	go test -tags load -run TestLoadBrowserTerminalHub -v ./internal/edgehttp/

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadBrowserTerminalHub(t *testing.T) {
	for _, shape := range []struct{ terminals, sharedViewers int }{{1, 1}, {1, 50}, {100, 1}, {100, 50}} {
		t.Run(fmt.Sprintf("terminals=%d/viewers=%d", shape.terminals, shape.sharedViewers), func(t *testing.T) {
			loadBrowserTerminalHub(t, shape.terminals, shape.sharedViewers)
		})
	}
}

func loadBrowserTerminalHub(t *testing.T, terminals, sharedViewers int) {
	const recordBytes = 4 << 10
	const duration = 5 * time.Second
	hub := NewBrowserTerminalHub()
	defer hub.Close()
	identity := testEdgePreviewIdentity(1, 1)
	fence := BrowserTerminalRouteFence{Identity: identity, Revision: 1, AttachmentGeneration: 1}
	type viewer struct {
		sub       *BrowserTerminalSubscription
		latencies []time.Duration
		records   int
		lagged    int
	}
	var publishers []*BrowserTerminalPublisher
	var viewers []*viewer
	var stalled *BrowserTerminalSubscription
	for terminal := 0; terminal < terminals; terminal++ {
		route := fmt.Sprintf("route_%03d", terminal)
		if err := hub.ActivateRoute(route, fence); err != nil {
			t.Fatal(err)
		}
		key := BrowserTerminalSessionKey{RouteID: route, TerminalSessionID: fmt.Sprintf("term_%03d", terminal), ProcessGeneration: identity.ProcessGeneration}
		count := 1
		if terminal == 0 {
			count = sharedViewers
			sub, err := hub.Subscribe(key, fence, "attach_stalled")
			if err != nil {
				t.Fatal(err)
			}
			stalled = sub
		}
		for index := 0; index < count; index++ {
			sub, err := hub.Subscribe(key, fence, fmt.Sprintf("attach_%03d_%02d", terminal, index))
			if err != nil {
				t.Fatalf("terminal %d viewer %d: %v", terminal, index, err)
			}
			viewers = append(viewers, &viewer{sub: sub})
		}
		publisher, err := hub.RegisterPublisher(key, identity)
		if err != nil {
			t.Fatal(err)
		}
		publishers = append(publishers, publisher)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration+5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, v := range viewers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				record, err := v.sub.Next(ctx)
				if errors.Is(err, ErrBrowserTerminalLagged) {
					v.lagged++
					continue
				}
				if err != nil {
					return
				}
				v.records++
				sent := time.Unix(0, int64(binary.BigEndian.Uint64(record)))
				if v.records%16 == 0 {
					v.latencies = append(v.latencies, time.Since(sent))
				}
			}
		}()
	}
	var published, failed atomic.Int64
	deadline := time.Now().Add(duration)
	var publishing sync.WaitGroup
	for _, publisher := range publishers {
		publishing.Add(1)
		go func() {
			defer publishing.Done()
			record := make([]byte, recordBytes)
			// Real output rates: a terminal flood is ≤ 20 MiB/s, an edge node's NIC ≤ 200 MiB/s across terminals.
			rate := min(20<<20, (200<<20)/len(publishers)) / recordBytes
			interval := time.Second / time.Duration(rate)
			next := time.Now()
			for time.Now().Before(deadline) {
				next = next.Add(interval)
				if wait := time.Until(next); wait > 0 {
					time.Sleep(wait)
				}
				binary.BigEndian.PutUint64(record, uint64(time.Now().UnixNano()))
				if err := publisher.Publish(append([]byte(nil), record...)); err != nil {
					failed.Add(1)
					return
				}
				published.Add(1)
			}
		}()
	}
	publishing.Wait()
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	var delivered int
	var latencies []time.Duration
	cutOff := 0
	for _, v := range viewers {
		delivered += v.records
		latencies = append(latencies, v.latencies...)
		if v.lagged > 0 {
			cutOff++
		}
	}
	slices.Sort(latencies)
	p := func(q float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)].Round(time.Microsecond)
	}
	// A viewer that never reads holds exactly one lag marker once its backlog was dropped.
	stalledCut := false
	if _, err := stalled.Next(context.Background()); errors.Is(err, ErrBrowserTerminalLagged) {
		stalledCut = true
	}
	perPublisher := float64(published.Load()) / duration.Seconds() / float64(len(publishers))
	t.Logf("published %d records (%.0f/s per terminal, %.1f MiB/s per terminal), delivered %d (%.0f/s total), latency p50 %v p99 %v max %v; reading viewers lagged %d of %d; stalled viewer lagged: %v",
		published.Load(), perPublisher, perPublisher*recordBytes/(1<<20), delivered, float64(delivered)/duration.Seconds(), p(.5), p(.99), p(1), cutOff, len(viewers), stalledCut)
	if cutOff > 0 {
		t.Errorf("%d of %d viewers that keep reading lagged at real output rates", cutOff, len(viewers))
	}
	if !stalledCut {
		t.Errorf("a viewer that never reads did not lag after %d records", published.Load())
	}
	if failed.Load() > 0 {
		t.Errorf("%d publishers failed", failed.Load())
	}
}
