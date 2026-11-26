//go:build linux || darwin

package updated

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/liveconnections"
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

// Signed updates follow the same rule as supplied ones: a candidate daemon only waiting for the
// control plane is waited for and commits once ready; still waiting at the deadline rolls back with
// ErrParticipantControlPlaneTimeout; degraded for another reason fails once it lasted ParticipantSettle (a new
// daemon retries its failing steps itself; a first timeout under load is not a verdict). Recovery to the
// previous version keeps waiting on any failure.
func TestSignedUpdateWaitsOnlyForTheControlPlane(t *testing.T) {
	settle := ParticipantSettle
	// Wide enough that two 200 ms probes fit on a slow CI runner (macos-latest missed 300 ms once).
	ParticipantSettle = 700 * time.Millisecond
	t.Cleanup(func() { ParticipantSettle = settle })
	handoff := &unixActivationHandoff{Baseline: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}, RequireInventory: true}
	waiting := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", ControlPlaneUnavailableOnly: true, Machines: 1}
	ready := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}
	local := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", Machines: 1}
	wait := func(probes []UnixParticipantProbe, candidate bool, deadline time.Duration) (time.Duration, error) {
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		defer cancel()
		started := time.Now()
		err := (&unixParticipantReadiness{participants: &sequencedParticipants{probes: probes}, handoff: handoff}).waitReady(ctx, "2", candidate)
		return time.Since(started), err
	}
	if _, err := wait([]UnixParticipantProbe{waiting, waiting, ready}, true, 2*time.Second); err != nil {
		t.Fatalf("candidate that reached the server after two degraded answers: %v", err)
	}
	if _, err := wait([]UnixParticipantProbe{waiting}, true, 700*time.Millisecond); !errors.Is(err, ErrParticipantControlPlaneTimeout) || !errors.Is(err, ErrParticipantReadiness) {
		t.Fatalf("candidate still waiting for the server at the deadline: %v", err)
	}
	if took, err := wait([]UnixParticipantProbe{local}, true, 2*time.Second); err == nil || errors.Is(err, ErrParticipantControlPlaneTimeout) || took > 1500*time.Millisecond {
		t.Fatalf("locally degraded candidate: err=%v after %v, want a failure once the settle window passed", err, took)
	}
	if _, err := wait([]UnixParticipantProbe{local, local, ready}, true, 2*time.Second); err != nil {
		t.Fatalf("a candidate that recovers inside the settle window must commit: %v", err)
	}
	if _, err := wait([]UnixParticipantProbe{local, local, ready}, false, 2*time.Second); err != nil {
		t.Fatalf("recovery must keep waiting through a transient local degradation: %v", err)
	}
}

// The check after signed health uses the same settle as the one before it: a daemon briefly degraded
// while it reconciles with a restarted machine guard commits once ready (canary 2026-11-26 rolled back).
func TestValidateReadyToleratesTransientDegradedAfterHealth(t *testing.T) {
	settle := ParticipantSettle
	ParticipantSettle = 2 * time.Second
	t.Cleanup(func() { ParticipantSettle = settle })
	handoff := &unixActivationHandoff{Baseline: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}, RequireInventory: true}
	ready := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}
	local := UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", Machines: 1}
	readiness := &unixParticipantReadiness{participants: &sequencedParticipants{probes: []UnixParticipantProbe{ready, local, local, ready}}, handoff: handoff}
	validated := false
	err := readiness.validateReady(t.Context(), "2", 5*time.Second, func(context.Context) error { validated = true; return nil })
	if err != nil || !validated {
		t.Fatalf("transient degraded after signed health: validated=%v err=%v", validated, err)
	}
	stuck := &unixParticipantReadiness{participants: &sequencedParticipants{probes: []UnixParticipantProbe{ready, local}}, handoff: handoff}
	ParticipantSettle = 300 * time.Millisecond
	if err := stuck.validateReady(t.Context(), "2", 5*time.Second, func(context.Context) error { return nil }); !errors.Is(err, ErrParticipantReadiness) {
		t.Fatalf("daemon degraded past the settle window must still fail: %v", err)
	}
	// A daemon that never becomes ready again after health fails within one budget, not two.
	notRunning := UnixParticipantProbe{Running: false, Version: "2", UpdaterVersion: "2"}
	slow := &unixParticipantReadiness{participants: &sequencedParticipants{probes: []UnixParticipantProbe{notRunning, notRunning, ready, notRunning}}, handoff: handoff}
	started := time.Now()
	err = slow.validateReady(t.Context(), "2", 600*time.Millisecond, func(context.Context) error { return nil })
	if !errors.Is(err, ErrParticipantReadiness) || time.Since(started) > 850*time.Millisecond {
		t.Fatalf("never-ready candidate: err=%v after %v, want a failure within one budget", err, time.Since(started))
	}
}

// A machine removed from its account waits for enrollment before and after the update; that is its
// ready state (smoke 2026-11-26: every update of a removed machine rolled back on daemon_state). A
// removed machine never accepts a worse state, and a ready machine never accepts awaiting_enrollment.
func TestRemovedMachineUpdatesWhileAwaitingEnrollment(t *testing.T) {
	awaiting := UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "awaiting_enrollment"}
	next := awaiting
	next.Version, next.UpdaterVersion = "2", "2"
	// Preflight (activation_unix.go) checks the baseline against itself before approving.
	if err := unixParticipantReady(awaiting, "1", awaiting, false); err != nil {
		t.Fatalf("preflight of a removed machine: %v", err)
	}
	if err := unixParticipantReady(next, "2", awaiting, false); err != nil {
		t.Fatalf("removed machine: %v", err)
	}
	worse := next
	worse.State = "degraded"
	if err := unixParticipantReady(worse, "2", awaiting, false); err == nil {
		t.Fatal("removed machine accepted a degraded daemon")
	}
	ready := awaiting
	ready.State = "ready"
	if err := unixParticipantReady(next, "2", ready, true); err == nil {
		t.Fatal("ready machine accepted awaiting_enrollment")
	}
}
