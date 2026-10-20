package liveconnections

import "testing"

func TestCountsFollowOpenAndClosedConnections(t *testing.T) {
	base := Current()
	closeSSH := TrackSSH()
	closeForward := TrackForward()
	TrackSSH()()
	if got := Current(); got.SSH != base.SSH+1 || got.Forwards != base.Forwards+1 || got.Total() != base.Total()+2 {
		t.Fatalf("counts = %+v from %+v", got, base)
	}
	closeSSH()
	closeSSH() // closing twice must not undercount
	closeForward()
	if got := Current(); got != base {
		t.Fatalf("counts = %+v, want %+v", got, base)
	}
}
