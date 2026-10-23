//go:build linux || darwin

package updated

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

type sequencedParticipants struct {
	probes []UnixParticipantProbe // returned in order; the last repeats
	calls  int
}

func (s *sequencedParticipants) Probe(context.Context) (UnixParticipantProbe, error) {
	probe := s.probes[min(s.calls, len(s.probes)-1)]
	s.calls++
	return probe, nil
}
func (s *sequencedParticipants) Restart(context.Context) error { return nil }
func (s *sequencedParticipants) OpenConnections(context.Context) (liveconnections.Counts, error) {
	return liveconnections.Counts{}, nil
}

// Signed updates follow the same rule as supplied ones (Lane F): a candidate daemon only waiting for the
// control plane is waited for and commits once ready; still waiting at the deadline rolls back with
// ErrParticipantControlPlaneTimeout; degraded for another reason fails at once. Recovery to the previous
// version keeps waiting on any failure.
func TestSignedUpdateWaitsOnlyForTheControlPlane(t *testing.T) {
	handoff := &unixActivationHandoff{Baseline: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}, RequireInventory: true}
	waiting := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", ControlPlaneUnavailableOnly: true, Machines: 1}
	ready := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}
	local := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", Machines: 1}
	wait := func(probes []UnixParticipantProbe, candidate bool, deadline time.Duration) (error, time.Duration) {
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		defer cancel()
		started := time.Now()
		err := (&unixParticipantReadiness{participants: &sequencedParticipants{probes: probes}, handoff: handoff}).waitReady(ctx, "2", candidate)
		return err, time.Since(started)
	}
	if err, _ := wait([]UnixParticipantProbe{waiting, waiting, ready}, true, 2*time.Second); err != nil {
		t.Fatalf("candidate that reached the server after two degraded answers: %v", err)
	}
	if err, _ := wait([]UnixParticipantProbe{waiting}, true, 700*time.Millisecond); !errors.Is(err, ErrParticipantControlPlaneTimeout) || !errors.Is(err, ErrParticipantReadiness) {
		t.Fatalf("candidate still waiting for the server at the deadline: %v", err)
	}
	if err, took := wait([]UnixParticipantProbe{local}, true, 2*time.Second); err == nil || errors.Is(err, ErrParticipantControlPlaneTimeout) || took > time.Second {
		t.Fatalf("locally degraded candidate: err=%v after %v, want an immediate failure", err, took)
	}
	if err, _ := wait([]UnixParticipantProbe{local, local, ready}, false, 2*time.Second); err != nil {
		t.Fatalf("recovery must keep waiting through a transient local degradation: %v", err)
	}
}
