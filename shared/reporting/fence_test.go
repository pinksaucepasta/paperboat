package reporting

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestFenceRefusesSubmissionsAfterCloseAndCountsThem(t *testing.T) {
	var f Fence
	if !f.Begin(false) {
		t.Fatal("open fence refused a submission")
	}
	f.End()
	f.Close(context.Background(), time.Second, nil, func(context.Context) bool { return true })
	if f.Begin(false) || f.Begin(true) {
		t.Fatal("closed fence admitted a submission")
	}
	if f.Status() != "drained" || f.Dropped() != 2 || !f.Closed() {
		t.Fatalf("status=%s dropped=%d closed=%v", f.Status(), f.Dropped(), f.Closed())
	}
}

func TestFenceRunsPreCloseBeforeDrainAndAdmitsSamplersUntilSealed(t *testing.T) {
	var f Fence
	var order []string
	f.Close(context.Background(), time.Second, func(context.Context) bool {
		order = append(order, "preclose")
		if f.Begin(false) {
			t.Error("a non-sampler submission was admitted while closing")
		}
		if !f.Begin(true) {
			t.Error("the sampler was refused before the fence sealed")
		} else {
			f.End()
		}
		return true
	}, func(context.Context) bool {
		order = append(order, "drain")
		return true
	})
	if len(order) != 2 || order[0] != "preclose" || order[1] != "drain" || f.Status() != "drained" {
		t.Fatalf("order=%v status=%s", order, f.Status())
	}
}

func TestFenceTimesOutWhenDrainOrPreCloseOverruns(t *testing.T) {
	var drainedLate atomic.Bool
	var slow Fence
	slow.Close(context.Background(), 20*time.Millisecond, nil, func(ctx context.Context) bool {
		<-ctx.Done()
		drainedLate.Store(true)
		return false
	})
	if slow.Status() != "timed_out" {
		t.Fatalf("slow drain status = %s", slow.Status())
	}
	var stuck Fence
	stuck.Close(context.Background(), time.Second, func(context.Context) bool { return false }, func(context.Context) bool { return true })
	if stuck.Status() != "timed_out" {
		t.Fatalf("failed preClose status = %s", stuck.Status())
	}
}

func TestFenceCloseWaitsForAdmittedSubmissions(t *testing.T) {
	var f Fence
	if !f.Begin(false) {
		t.Fatal("not admitted")
	}
	drained := make(chan struct{})
	go f.Close(context.Background(), time.Second, nil, func(context.Context) bool { close(drained); return true })
	// Start the negative window once Close is provably waiting for the admitted submission (the fence sealed).
	for !f.sealed.Load() {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-drained:
		t.Fatal("drain ran while a submission was admitted")
	case <-time.After(50 * time.Millisecond):
	}
	f.End()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain never ran after the submission ended")
	}
}
