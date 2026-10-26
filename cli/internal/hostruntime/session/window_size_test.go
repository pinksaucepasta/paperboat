package session

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

var sizeNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func dims(c, r uint16) pty.Dimensions { return pty.Dimensions{Columns: c, Rows: r} }

func mustController(t *testing.T, w *windowSizes, wantID string, want pty.Dimensions) {
	t.Helper()
	id, size, ok := w.controller()
	if !ok || id != wantID || size != want {
		t.Fatalf("controller = %q %v (ok %v), want %q %v", id, size, ok, wantID, want)
	}
}

// F3-17: the PTY follows the controller window that was last active; attaching and reconnecting never take control.
func TestWindowSizesFollowTheLatestActiveController(t *testing.T) {
	w := newWindowSizes()
	if _, _, ok := w.controller(); ok {
		t.Fatal("a session with no window has a controller")
	}
	w.attach("a", dims(100, 30), sizeNow)
	w.attach("b", dims(80, 24), sizeNow)
	mustController(t, w, "a", dims(100, 30)) // nobody active yet: the first attachment controls

	if !w.touch("b", sizeNow) {
		t.Fatal("typing in a second window did not change the controller")
	}
	mustController(t, w, "b", dims(80, 24))

	w.report("a", dims(120, 40)) // a resizes but is not active: stored, not applied
	mustController(t, w, "b", dims(80, 24))

	if w.touch("b", sizeNow.Add(time.Millisecond)) {
		t.Fatal("typing in the controller changed the controller")
	}
	if !w.touch("a", sizeNow.Add(time.Second)) {
		t.Fatal("typing in window a did not change the controller")
	}
	mustController(t, w, "a", dims(120, 40)) // a is active now and its stored size applies

	w.report("a", dims(130, 50)) // the active controller resizing applies at once
	mustController(t, w, "a", dims(130, 50))
}

func TestAReconnectNeverStealsControl(t *testing.T) {
	w := newWindowSizes()
	w.attach("a", dims(100, 30), sizeNow)
	w.attach("b", dims(80, 24), sizeNow)
	w.touch("a", sizeNow)
	w.touch("b", sizeNow.Add(time.Second))
	mustController(t, w, "b", dims(80, 24))

	// The connection of window a drops and the same attachment id comes back with a new size: its activity is kept,
	// so b still controls.
	w.detach("a", sizeNow.Add(2*time.Second))
	w.attach("a", dims(110, 35), sizeNow.Add(3*time.Second))
	mustController(t, w, "b", dims(80, 24))

	// The controller itself reconnects, with a new size: it stays the controller and its new size applies.
	w.detach("b", sizeNow.Add(4*time.Second))
	mustController(t, w, "a", dims(110, 35)) // while b is away the other window controls
	w.attach("b", dims(90, 30), sizeNow.Add(5*time.Second))
	mustController(t, w, "b", dims(90, 30))
}

func TestADetachedWindowForgetsItsActivityAfterTheRetention(t *testing.T) {
	w := newWindowSizes()
	w.attach("a", dims(100, 30), sizeNow)
	w.attach("b", dims(80, 24), sizeNow)
	w.touch("b", sizeNow)
	w.detach("b", sizeNow)
	// Long after, b comes back as a stranger: its old activity no longer outranks a's.
	w.attach("b", dims(80, 24), sizeNow.Add(sizeWindowRetention+time.Minute))
	mustController(t, w, "a", dims(100, 30))
}

func TestTheNextControllerTakesOverWhenTheActiveOneLeaves(t *testing.T) {
	w := newWindowSizes()
	w.attach("a", dims(100, 30), sizeNow)
	w.attach("b", dims(80, 24), sizeNow)
	w.attach("c", dims(90, 28), sizeNow)
	w.touch("a", sizeNow)
	w.touch("b", sizeNow.Add(time.Second))
	w.touch("c", sizeNow.Add(2*time.Second))
	mustController(t, w, "c", dims(90, 28))
	w.detach("c", sizeNow)
	mustController(t, w, "b", dims(80, 24)) // the latest of the rest
	w.detach("b", sizeNow)
	mustController(t, w, "a", dims(100, 30))
	w.detach("a", sizeNow)
	if _, _, ok := w.controller(); ok {
		t.Fatal("a session with every window gone has a controller")
	}
}

func TestEqualActivityNeverFlaps(t *testing.T) {
	for range 20 {
		w := newWindowSizes()
		w.attach("b", dims(80, 24), sizeNow)
		w.attach("a", dims(100, 30), sizeNow)
		w.touch("a", sizeNow)
		w.touch("b", sizeNow)
		mustController(t, w, "a", dims(100, 30))
	}
}

func TestAnUnreportedSizeIsNeverAControllerSize(t *testing.T) {
	w := newWindowSizes()
	w.attach("a", dims(0, 0), sizeNow)
	if _, _, ok := w.controller(); ok {
		t.Fatal("a window without a size controls")
	}
	w.report("a", dims(0, 24))
	if _, _, ok := w.controller(); ok {
		t.Fatal("a half-reported size controls")
	}
	w.report("a", dims(80, 24))
	mustController(t, w, "a", dims(80, 24))
}

func TestAnUnknownAttachmentIsIgnored(t *testing.T) {
	w := newWindowSizes()
	w.attach("a", dims(80, 24), sizeNow)
	if w.touch("unknown", sizeNow) {
		t.Fatal("an unknown attachment changed the controller")
	}
	w.report("unknown", dims(200, 60))
	w.detach("unknown", sizeNow)
	mustController(t, w, "a", dims(80, 24))
}

// Touch is called from the input path under concurrent attach and resize; it must be race free (go test -race).
func TestWindowSizesAreRaceFree(t *testing.T) {
	w := newWindowSizes()
	var wg sync.WaitGroup
	for i, id := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 500 {
				w.attach(id, dims(uint16(80+i), 24), time.Unix(int64(n), 0))
				w.touch(id, time.Unix(int64(n), 0))
				w.report(id, dims(uint16(90+i), 30))
				_, _, _ = w.controller()
				w.detach(id+strconv.Itoa(n%2), time.Unix(int64(n), 0))
				w.detach(id, time.Unix(int64(n), 0))
			}
		}()
	}
	wg.Wait()
}
