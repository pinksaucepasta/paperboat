package session

import (
	"errors"
	"io"
	"sync"
	"testing"
)

type countingWriter struct {
	mu    sync.Mutex
	calls int
	n     int
	err   error
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.n < 0 {
		return len(p), w.err
	}
	return w.n, w.err
}

func TestInputDuplicateWritesExactlyOnce(t *testing.T) {
	j := NewInputJournal(1)
	w := &countingWriter{n: -1}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputID: "inp"}
	first, err := j.Write(key, []byte("ls\n"), w)
	if err != nil || first.Status != InputAccepted {
		t.Fatalf("decision=%#v err=%v", first, err)
	}
	second, err := j.Write(key, []byte("ls\n"), w)
	if err != nil || second.Status != InputDuplicate || w.calls != 1 {
		t.Fatalf("decision=%#v calls=%d err=%v", second, w.calls, err)
	}
	if _, err := j.Write(key, []byte("pwd\n"), w); !errors.Is(err, ErrInputConflict) {
		t.Fatalf("conflict err=%v", err)
	}
}

func TestInputGenerationAndUncertainWrite(t *testing.T) {
	j := NewInputJournal(2)
	stale := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputID: "old"}
	if _, err := j.Write(stale, []byte("x"), &countingWriter{n: -1}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale err=%v", err)
	}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 2, InputID: "new"}
	decision, err := j.Write(key, []byte("abc"), &countingWriter{n: 1, err: io.ErrUnexpectedEOF})
	if err != nil || decision.Status != InputUncertain || decision.WriteError != "pty_write_failed" {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	queried, err := j.Query(key)
	if err != nil || queried.Status != InputUncertain {
		t.Fatalf("query=%#v err=%v", queried, err)
	}
}

func TestConcurrentDuplicateInputWritesOnce(t *testing.T) {
	j := NewInputJournal(1)
	w := &countingWriter{n: -1}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputID: "same"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := j.Write(key, []byte("x"), w); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if w.calls != 1 {
		t.Fatalf("calls=%d", w.calls)
	}
}

func TestBoundedInputJournalRejectsNewIdentityButPreservesDuplicate(t *testing.T) {
	journal := NewBoundedInputJournal(1, 1)
	writer := &countingWriter{n: -1}
	first := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputID: "input_1"}
	if decision, err := journal.Write(first, []byte("a"), writer); err != nil || decision.Status != InputAccepted {
		t.Fatalf("first=%#v err=%v", decision, err)
	}
	second := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputID: "input_2"}
	if _, err := journal.Write(second, []byte("b"), writer); !errors.Is(err, ErrInputJournalFull) {
		t.Fatalf("full err=%v", err)
	}
	if decision, err := journal.Write(first, []byte("a"), writer); err != nil || decision.Status != InputDuplicate {
		t.Fatalf("duplicate=%#v err=%v", decision, err)
	}
	if writer.calls != 1 {
		t.Fatalf("writer calls=%d", writer.calls)
	}
}

func TestInputSequenceReservesBeforePTYWriteAndRejectsUncertainSuccessor(t *testing.T) {
	journal := NewInputJournal(1)
	writer := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputSequence: 1}
	decision := make(chan InputDecision, 1)
	go func() {
		value, err := journal.Write(key, []byte("x"), writer)
		if err != nil {
			t.Errorf("write err=%v", err)
		}
		decision <- value
	}()
	<-writer.started
	queried, err := journal.Query(key)
	if err != nil || queried.Status != InputUncertain || queried.InputSequence != 1 {
		t.Fatalf("reserved decision=%#v err=%v", queried, err)
	}
	if _, err := journal.Write(InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputSequence: 2}, []byte("y"), &countingWriter{n: -1}); !errors.Is(err, ErrInputUncertain) {
		t.Fatalf("successor err=%v, want uncertain", err)
	}
	close(writer.release)
	if got := <-decision; got.Status != InputAccepted {
		t.Fatalf("final decision=%#v", got)
	}
}

func TestInputSequenceDuplicateReturnsOriginalDecision(t *testing.T) {
	journal := NewInputJournal(1)
	writer := &countingWriter{n: -1}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1, InputSequence: 1}
	first, err := journal.Write(key, []byte("x"), writer)
	if err != nil || first.Status != InputAccepted {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := journal.Write(key, []byte("x"), writer)
	if err != nil || second.Status != InputAccepted || second.InputSequence != 1 || writer.calls != 1 {
		t.Fatalf("duplicate=%#v calls=%d err=%v", second, writer.calls, err)
	}
}

// A terminal session takes input for its whole life: sequenced decisions are kept only for the recent
// window, so input is never refused for having taken many writes (Lane X audit H2: refused after
// MaxInputDecisions), a resend inside the window is still answered from the journal, and a sequence that
// left the window is refused, never written twice. A new process generation drops the old decisions.
func TestInputJournalKeepsARecentWindowForLongSessions(t *testing.T) {
	j := NewBoundedInputJournal(1, 2*InputDecisionWindow)
	w := &countingWriter{n: -1}
	key := InputKey{ClientID: "cli", AttachmentID: "att", Generation: 1}
	for sequence := uint64(1); sequence <= 10_000; sequence++ {
		key.InputSequence = sequence
		if decision, err := j.Write(key, []byte("k"), w); err != nil || decision.Status != InputAccepted {
			t.Fatalf("write %d: decision=%+v err=%v", sequence, decision, err)
		}
	}
	key.InputSequence = 10_000
	if decision, err := j.Write(key, []byte("k"), w); err != nil || decision.Status != InputAccepted || w.calls != 10_000 {
		t.Fatalf("resend inside the window: decision=%+v calls=%d err=%v", decision, w.calls, err)
	}
	key.InputSequence = 10_000 - InputDecisionWindow
	if _, err := j.Write(key, []byte("k"), w); !errors.Is(err, ErrInputSequence) || w.calls != 10_000 {
		t.Fatalf("a sequence outside the window was not refused: err=%v calls=%d", err, w.calls)
	}
	j.SetGeneration(2)
	if len(j.decisions) != 0 || len(j.last) != 0 {
		t.Fatalf("old generation kept: decisions=%d cursors=%d", len(j.decisions), len(j.last))
	}
}
