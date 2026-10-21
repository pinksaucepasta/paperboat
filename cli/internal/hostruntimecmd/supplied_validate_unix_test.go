//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
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
