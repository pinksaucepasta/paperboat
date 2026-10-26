package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
)

// Lane W scale matrix: with 1,000 endpoint routes and no change, the edge takes the full set only
// when its full interval comes round and otherwise only deltas (an empty delta per poll), and a
// single new route is installed within a couple of polls. The full set's size at 1k and 10k
// endpoints is logged for the scale matrix.
func TestEndpointPullTakesDeltasBetweenFullSets(t *testing.T) {
	runtimeRoute := runtimeAdmission(t)
	endpoint := func(i int) runtimewire.Admission {
		a := endpointAdmission(runtimeRoute, "public")
		a.Binding.RouteID, a.EndpointID = fmt.Sprintf("route_%05d", i), fmt.Sprintf("preview_%05d", i)
		a.Hostname = fmt.Sprintf("endpoint-%05d.preview.example.test", i)
		return a
	}
	all := []runtimewire.Admission{}
	for i := 0; i < 1000; i++ {
		all = append(all, endpoint(i))
	}
	// The grouped full answer (each carrier's admission once): total and largest 500-row page.
	for _, carriers := range []int{1, 1000, 10000} {
		set := make([]runtimewire.Admission, 10000)
		for i := range set {
			set[i] = endpoint(i)
			set[i].Binding.SessionID = fmt.Sprintf("session_%05d", i/(10000/carriers)) // server order: carrier, then route
		}
		total, largest := 0, 0
		for start := 0; start < len(set); start += 500 {
			groups := runtimewire.GroupEndpointAdmissions(set[start:min(start+500, len(set))])
			raw, _ := json.Marshal(runtimewire.Snapshot{Schema: runtimewire.Schema, Complete: true, Carriers: &groups})
			total += len(raw)
			largest = max(largest, len(raw))
		}
		t.Logf("full endpoint answer, 10,000 endpoints on %d carriers: %d bytes in 20 pages, largest page %d bytes", carriers, total, largest)
	}
	var added *runtimewire.Admission
	source := &runtimeSourceFake{admissions: []runtimewire.Admission{runtimeRoute}}
	source.endpointUpdate = func(since string) control.EndpointUpdate {
		if since == "" {
			return control.EndpointUpdate{Complete: true, Admissions: append([]runtimewire.Admission(nil), all...), Cursor: "c"}
		}
		delta := control.EndpointUpdate{Admissions: []runtimewire.Admission{}, Cursor: "c", ExpiresAt: time.Now().Add(time.Hour)}
		if added != nil {
			delta.Admissions = append(delta.Admissions, *added)
		}
		return delta
	}
	epoch := runtimeRoute.Binding.EdgeProcessEpoch
	expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: runtimeRoute.Binding.EdgeNodeID, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	w := &RuntimeCarrierWorker{Source: source, Expected: expected, Routes: endpointRegistry(t, epoch), Endpoints: endpointRegistry(t, epoch),
		Interval: time.Minute, EndpointInterval: 5 * time.Millisecond, EndpointFullInterval: 300 * time.Millisecond, Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerStarted := time.Now()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Shutdown(context.Background()) }()
	// Installing 1,000 admissions validates each; give a loaded host more than waitRuntime's 3 s.
	for deadline := time.Now().Add(15 * time.Second); len(expected.Snapshot()) != 1001; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("full endpoint set not installed: %d admissions", len(expected.Snapshot()))
		}
	}
	// Wait for 20 deltas (a loaded host is slow), then require only the full sets the elapsed time
	// allows: one to start and one per full interval, so deltas carry the rest.
	count := func() (full, deltas int) {
		for _, since := range source.endpointPulls {
			if since == "" {
				full++
			} else {
				deltas++
			}
		}
		return full, deltas
	}
	var full, deltas int
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		source.mu.Lock()
		full, deltas = count()
		if deltas >= 20 || time.Now().After(deadline) {
			break
		}
		source.mu.Unlock()
	}
	extra := endpoint(1000)
	added = &extra
	source.mu.Unlock()
	allowed := 2 + int(time.Since(workerStarted)/w.EndpointFullInterval)
	if full > allowed || deltas < 20 {
		t.Fatalf("endpoint pulls: %d full (at most %d), %d deltas", full, allowed, deltas)
	}
	started := time.Now()
	waitRuntime(t, func() bool { return len(expected.Snapshot()) == 1002 })
	t.Logf("%d full sets and %d deltas; a new route installed after %v", full, deltas, time.Since(started))
}
