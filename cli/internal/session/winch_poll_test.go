package session

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollWinchNotifiesOnChangeAndCoalescesAStorm(t *testing.T) {
	var cols atomic.Int64
	var failing atomic.Bool
	cols.Store(80)
	size := func() (int, int, error) {
		if failing.Load() {
			return 0, 0, errors.New("no console")
		}
		return int(cols.Load()), 24, nil
	}
	ch := make(chan os.Signal, 1)
	tick := make(chan time.Time) // unbuffered: a taken tick means the previous one is fully handled
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() { pollWinch(ch, size, tick, done); close(exited) }()
	step := func() { tick <- time.Time{}; tick <- time.Time{} }
	pending := func() int { return len(ch) }

	step()
	if pending() != 0 {
		t.Fatal("notified without a size change")
	}
	cols.Store(109)
	step()
	if pending() != 1 {
		t.Fatal("no notification after the console was resized")
	}
	<-ch
	step()
	if pending() != 0 {
		t.Fatal("notified again for the same size")
	}
	failing.Store(true)
	step()
	failing.Store(false)
	if pending() != 0 {
		t.Fatal("notified for a failed size read")
	}
	for _, c := range []int64{90, 100, 132} { // storm: nobody drains ch
		cols.Store(c)
		step()
	}
	if pending() != 1 {
		t.Fatalf("storm left %d notifications, want 1", pending())
	}
	<-ch
	if w, _, _ := size(); w != 132 {
		t.Fatalf("size read on the notification = %d, want the last one, 132", w)
	}
	close(done)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("poller still running after stop")
	}
}
