package server

import "testing"

// A frame queued for the worker counts as busy before the worker has started on it: a paste whose frames all arrive
// before the worker runs must not make the Ctrl-C behind it look idle (it would queue behind writes that never finish).
func TestFrameBacklogIsBusyFromQueueingUntilServed(t *testing.T) {
	var backlog frameBacklog
	if backlog.busy() {
		t.Fatal("an empty backlog is busy")
	}
	backlog.begin()
	backlog.begin()
	if !backlog.busy() {
		t.Fatal("frames queued before the worker started read as idle")
	}
	backlog.end()
	if !backlog.busy() {
		t.Fatal("a frame still waiting reads as idle")
	}
	backlog.end()
	if backlog.busy() {
		t.Fatal("a served backlog is still busy")
	}
}
