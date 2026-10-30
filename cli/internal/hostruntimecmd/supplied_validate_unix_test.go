//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

type scriptedParticipants struct {
	restartErr error
	restarts   int
	probes     []updated.UnixParticipantProbe // returned in order; the last repeats
	probeCalls int
}

func (s *scriptedParticipants) Probe(context.Context) (updated.UnixParticipantProbe, error) {
	probe := s.probes[min(s.probeCalls, len(s.probes)-1)]
	s.probeCalls++
	return probe, nil
}
func (s *scriptedParticipants) Restart(context.Context) error { s.restarts++; return s.restartErr }
func (s *scriptedParticipants) OpenConnections(context.Context) (liveconnections.Counts, error) {
	return liveconnections.Counts{}, nil
}

// F14-03: a supplied update proves the user's daemon healthy on the new version before it commits;
// a daemon that cannot start or never reports the new version fails the install, which then restores
// the previous version.
func TestSuppliedUpdateIsValidatedAgainstTheUserDaemonBeforeItCommits(t *testing.T) {
	previous := suppliedUpdateReadyTimeout
	suppliedUpdateReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { suppliedUpdateReadyTimeout = previous })
	baseline := updated.UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}
	ready := updated.UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}

	healthy := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "starting"}, ready}}
	if err := restartAndAwaitDaemon(t.Context(), healthy, "2", baseline); err != nil || healthy.restarts != 1 {
		t.Fatalf("a daemon that starts on the new version failed validation: %v restarts=%d", err, healthy.restarts)
	}

	// An update begun during replacement must not accept a new daemon that has
	// not finished its first reconciliation, even if the old daemon was degraded.
	for _, state := range []string{"starting", "degraded"} {
		before := baseline
		before.State = state
		starting := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "starting"}, ready}}
		if err := restartAndAwaitDaemon(t.Context(), starting, "2", before); err != nil || starting.probeCalls != 2 {
			t.Fatalf("baseline %s completed before reconciliation: probes=%d err=%v", state, starting.probeCalls, err)
		}
	}

	cannotRestart := &scriptedParticipants{probes: []updated.UnixParticipantProbe{ready}, restartErr: errors.New("systemctl --user restart failed")}
	if err := restartAndAwaitDaemon(t.Context(), cannotRestart, "2", baseline); err == nil {
		t.Fatal("a daemon that cannot be restarted passed validation")
	}

	stillOld := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "1", UpdaterVersion: "2", State: "ready", Machines: 1}}}
	if err := restartAndAwaitDaemon(t.Context(), stillOld, "2", baseline); err == nil {
		t.Fatal("a daemon still on the old version passed validation")
	}

	notRunning := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{UpdaterVersion: "2"}}}
	if err := restartAndAwaitDaemon(t.Context(), notRunning, "2", baseline); err == nil {
		t.Fatal("a daemon that is not running passed validation")
	}

	// Already unhealthy for a local reason before the update: the new version only has to run.
	unhealthy := updated.UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 1}
	degradedNew := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", Machines: 1}}}
	if err := restartAndAwaitDaemon(t.Context(), degradedNew, "2", unhealthy); err != nil {
		t.Fatalf("updating an already degraded daemon was blocked: %v", err)
	}
}

func TestUpdateNotReadyKeepsItsOwnExitStatusAcrossElevation(t *testing.T) {
	refusal := hostinstallFailureForTest()
	if got := serviceFailureExitCode(refusal); got != UpdateNotReadyExitCode {
		t.Fatalf("exit status = %d, want %d", got, UpdateNotReadyExitCode)
	}
}

func hostinstallFailureForTest() error {
	return errors.Join(errors.New("validate"), hostinstall.ErrUpdateNotReady)
}

// A supplied install whose validation restarted the user's daemon and then failed restored the previous
// files but left that daemon on the replaced binary (lanel-b, 2026-10-22). After a failed install the
// daemon is restarted on the restored version, and only when validation had restarted it.
func TestFailedSuppliedInstallRestartsTheDaemonOnTheRestoredVersion(t *testing.T) {
	previous := suppliedUpdateReadyTimeout
	suppliedUpdateReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { suppliedUpdateReadyTimeout = previous })
	baseline := updated.UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}
	notReady := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", ControlPlaneUnavailableOnly: true, Machines: 1}}}
	stage, validate, recover := daemonValidation(notReady, "2", baseline)
	if err := recover(t.Context()); err != nil || notReady.restarts != 0 {
		t.Fatalf("install failed before validation: recover err=%v restarts=%d, want no restart", err, notReady.restarts)
	}
	if err := stage(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := validate(t.Context()); err == nil {
		t.Fatal("fixture: validation passed")
	}
	if err := recover(t.Context()); err != nil || notReady.restarts != 2 {
		t.Fatalf("after failed validation: recover err=%v restarts=%d, want the daemon restarted again on the restored version", err, notReady.restarts)
	}
	broken := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}}}
	stage, validate, recover = daemonValidation(broken, "2", baseline)
	if err := stage(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = validate(t.Context())
	broken.restartErr = errors.New("systemctl --user restart failed")
	if err := recover(t.Context()); err == nil {
		t.Fatal("a daemon that could not be restarted on the restored version was not reported")
	}
}

// Lane F's rule: a daemon that was ready must be ready again before an update commits. When it only
// waits for the control plane (a slow server), validation keeps polling until the deadline instead of
// failing; when the deadline passes in that state the error names the control plane; any other
// degradation fails at once.
func TestSuppliedUpdateWaitsOnlyForTheControlPlane(t *testing.T) {
	previous := suppliedUpdateReadyTimeout
	suppliedUpdateReadyTimeout = 2 * time.Second
	t.Cleanup(func() { suppliedUpdateReadyTimeout = previous })
	baseline := updated.UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 1}
	waiting := updated.UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", ControlPlaneUnavailableOnly: true, Machines: 1}
	ready := updated.UnixParticipantProbe{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}

	recovers := &scriptedParticipants{probes: []updated.UnixParticipantProbe{waiting, waiting, ready}}
	if err := restartAndAwaitDaemon(t.Context(), recovers, "2", baseline); err != nil {
		t.Fatalf("daemon that reached the server after two degraded answers: %v", err)
	}

	neverAnswers := &scriptedParticipants{probes: []updated.UnixParticipantProbe{waiting}}
	if err := restartAndAwaitDaemon(t.Context(), neverAnswers, "2", baseline); !errors.Is(err, updated.ErrParticipantControlPlaneTimeout) {
		t.Fatalf("daemon still waiting for the server at the deadline: %v, want the control-plane error", err)
	}

	locallyBroken := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "degraded", Machines: 1, Health: []string{"local_access_unavailable"}}}}
	started := time.Now()
	err := restartAndAwaitDaemon(t.Context(), locallyBroken, "2", baseline)
	if err == nil || errors.Is(err, updated.ErrParticipantControlPlaneTimeout) || time.Since(started) > time.Second {
		t.Fatalf("locally degraded daemon: err=%v after %v, want an immediate non-control-plane failure", err, time.Since(started))
	}
	// The rollback names the step that failed, not only that the service did not start.
	if !strings.Contains(err.Error(), "local access unavailable") {
		t.Fatalf("locally degraded daemon error does not name the failing step: %v", err)
	}
}

// Exercise the complete readiness policy while keeping the restart explicit at
// the staging boundary in the supplied-update orchestration tests.
func restartAndAwaitDaemon(ctx context.Context, p updated.UnixParticipants, version string, baseline updated.UnixParticipantProbe) error {
	if err := p.Restart(ctx); err != nil {
		return err
	}
	return awaitDaemon(ctx, p, version, baseline)
}

func TestSuppliedDaemonStartsBeforeCutoverAndValidationDoesNotRestartIt(t *testing.T) {
	p := &scriptedParticipants{probes: []updated.UnixParticipantProbe{{Running: true, Version: "2", UpdaterVersion: "2", State: "ready", Machines: 1}}}
	stage, validate, _ := daemonValidation(p, "2", updated.UnixParticipantProbe{Running: true, Version: "1", State: "ready", Machines: 1})
	if err := stage(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.restarts != 1 {
		t.Fatal("routing daemon was deferred until after the runtime cutover")
	}
	if err := validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.restarts != 1 {
		t.Fatal("validation restarted the already staged daemon")
	}
}
