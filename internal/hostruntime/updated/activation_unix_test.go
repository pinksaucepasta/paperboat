//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func TestUnixParticipantReadinessPreservesUsableBaseline(t *testing.T) {
	ready := UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 2}
	offline := UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", ControlPlaneUnavailableOnly: true}
	cases := []struct {
		name            string
		baseline, probe UnixParticipantProbe
		inventory, ok   bool
	}{
		{"connected", ready, ready, true, true},
		{"connected cannot become empty offline", ready, offline, true, false},
		{"empty offline remains locally usable", offline, offline, false, true},
		{"empty offline recovers online", offline, ready, false, true},
		{"other degradation is not readiness", offline, UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded"}, false, false},
		{"cached inventory must recover", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2, ControlPlaneUnavailableOnly: true}, offline, true, false},
		{"wrong daemon version", ready, UnixParticipantProbe{Running: true, Version: "0", UpdaterVersion: "1", State: "ready"}, true, false},
		{"wrong ordinary updater version", ready, UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "0", State: "ready"}, true, false},
		{"stopped daemon stays stopped", UnixParticipantProbe{}, UnixParticipantProbe{UpdaterVersion: "1"}, false, true},
		{"stopped daemon still requires updater", UnixParticipantProbe{}, UnixParticipantProbe{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := unixParticipantReady(tc.probe, "1", tc.baseline, tc.inventory)
			if (err == nil) != tc.ok {
				t.Fatalf("readiness=%v, want usable=%v", err, tc.ok)
			}
		})
	}
}

type participantFixture struct {
	probe    UnixParticipantProbe
	restarts int
	probeErr error
}

func (p *participantFixture) Probe(context.Context) (UnixParticipantProbe, error) {
	return p.probe, p.probeErr
}
func (p *participantFixture) Restart(context.Context) error { p.restarts++; return nil }

type controllerFixture struct {
	restartErr error
	retireErr  error
	restarts   int
}

func (c *controllerFixture) Install(context.Context, string, map[string]string) error { return nil }
func (c *controllerFixture) Retire(context.Context) error                             { return c.retireErr }
func (c *controllerFixture) RestartUpdater(context.Context) error                     { c.restarts++; return c.restartErr }
func (c *controllerFixture) RestartHostd(context.Context) error                       { return nil }

type gateFixture struct {
	started, stopped chan struct{}
	active           func(context.Context) error
	rollbacks        int
	drains           int
	drainErr         error
}

func (g *gateFixture) Candidate(context.Context, workerupdate.GateRequest) error { return nil }
func (g *gateFixture) Drain(context.Context, workerupdate.GateRequest) error {
	g.drains++
	return g.drainErr
}
func (g *gateFixture) Commit(context.Context, workerupdate.GateRequest) error { return nil }
func (g *gateFixture) Rollback(context.Context, workerupdate.GateRequest) error {
	g.rollbacks++
	return nil
}
func (g *gateFixture) Active(ctx context.Context, _ workerupdate.GateRequest) error {
	if g.started != nil {
		close(g.started)
	}
	if g.stopped != nil {
		defer close(g.stopped)
	}
	return g.active(ctx)
}

func TestUnixActivationUpdaterFailureCancelsWorkerObservation(t *testing.T) {
	failure := errors.New("candidate updater cannot start")
	inner := &gateFixture{started: make(chan struct{}), stopped: make(chan struct{}), active: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	controller := &controllerFixture{restartErr: failure}
	participants := &participantFixture{}
	handoff := &unixActivationHandoff{}
	gate := &unixParticipantGate{inner: inner, controller: controller, participants: participants, handoff: handoff, persist: func() error { return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := gate.Active(ctx, workerupdate.GateRequest{Candidate: workerupdate.Release{Version: "2"}})
	if !errors.Is(err, failure) || !handoff.ParticipantsTouched {
		t.Fatalf("failure=%v, touched=%v", err, handoff.ParticipantsTouched)
	}
	select {
	case <-inner.stopped:
	default:
		t.Fatal("worker observation outlived failed local activation")
	}
	if participants.restarts != 0 {
		t.Fatal("daemon restarted after updater failure")
	}
}

func TestUnixActivationRequiresLocalRecoveryBeforeRollbackAccepted(t *testing.T) {
	inner := &gateFixture{}
	participants := &participantFixture{probe: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "2", State: "ready"}}
	controller := &controllerFixture{}
	gate := &unixParticipantGate{inner: inner, controller: controller, participants: participants, handoff: &unixActivationHandoff{Baseline: UnixParticipantProbe{Running: true, State: "ready"}, RequireInventory: true, ParticipantsTouched: true}, persist: func() error { return nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := gate.Rollback(ctx, workerupdate.GateRequest{Previous: workerupdate.Release{Version: "2"}, Candidate: workerupdate.Release{Version: "1"}})
	if !errors.Is(err, ErrParticipantReadiness) || inner.rollbacks != 0 {
		t.Fatalf("rollback accepted unavailable updater: %v, inner=%d", err, inner.rollbacks)
	}
	participants.probe.UpdaterVersion = "1"
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := gate.Rollback(ctx2, workerupdate.GateRequest{Previous: workerupdate.Release{Version: "2"}, Candidate: workerupdate.Release{Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if inner.rollbacks != 1 || participants.restarts != 2 {
		t.Fatalf("rollback=%d daemon restarts=%d", inner.rollbacks, participants.restarts)
	}
}

func TestUnixActivationStrengthensPersistedBaselineBeforeDrain(t *testing.T) {
	offline := UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", ControlPlaneUnavailableOnly: true}
	participants := &participantFixture{probe: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready", Machines: 2}}
	inner := &gateFixture{}
	handoff := &unixActivationHandoff{Baseline: offline}
	persisted := false
	gate := &unixParticipantGate{inner: inner, participants: participants, handoff: handoff, persist: func() error { persisted = handoff.RequireInventory; return nil }}
	if err := gate.Drain(context.Background(), workerupdate.GateRequest{Previous: workerupdate.Release{Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if !persisted || inner.drains != 1 {
		t.Fatal("stronger pre-cutover baseline was not persisted before drain")
	}
	if err := unixParticipantReady(offline, "1", handoff.Baseline, handoff.RequireInventory); err == nil {
		t.Fatal("later offline state weakened connected baseline")
	}
}

func TestUnixActiveTerminalBusyRestoresUpdaterWithoutRestartingParticipants(t *testing.T) {
	busy := &autoupdate.ActiveTerminalSessionsError{RequiredVersion: "2"}
	inner := &gateFixture{drainErr: busy}
	participants := &participantFixture{probe: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready"}}
	controller := &controllerFixture{}
	handoff := &unixActivationHandoff{Baseline: participants.probe, RequireInventory: true}
	persists := 0
	gate := &unixParticipantGate{inner: inner, participants: participants, controller: controller, handoff: handoff, persist: func() error { persists++; return nil }}

	err := gate.Drain(context.Background(), workerupdate.GateRequest{Previous: workerupdate.Release{Version: "1"}})
	var gotBusy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &gotBusy) || gotBusy.RequiredVersion != "2" {
		t.Fatalf("drain=%v", err)
	}
	if !handoff.ActiveTerminalBusy || handoff.BusyRequiredVersion != "2" || handoff.BusyNextCheckAt.IsZero() || persists != 2 {
		t.Fatalf("handoff=%+v persists=%d", handoff, persists)
	}
	if err = gate.Rollback(context.Background(), workerupdate.GateRequest{Previous: workerupdate.Release{Version: "2"}, Candidate: workerupdate.Release{Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if controller.restarts != 1 || participants.restarts != 0 || inner.rollbacks != 1 || handoff.ParticipantsTouched {
		t.Fatalf("updater restarts=%d participant restarts=%d rollback=%d touched=%v", controller.restarts, participants.restarts, inner.rollbacks, handoff.ParticipantsTouched)
	}
}

func TestUnixActiveTerminalBusyPersistenceFailureIsNotBusy(t *testing.T) {
	persistErr := errors.New("handoff unavailable")
	inner := &gateFixture{drainErr: &autoupdate.ActiveTerminalSessionsError{RequiredVersion: "2"}}
	persists := 0
	gate := &unixParticipantGate{inner: inner, participants: &participantFixture{probe: UnixParticipantProbe{UpdaterVersion: "1"}}, controller: &controllerFixture{}, handoff: &unixActivationHandoff{}, persist: func() error {
		persists++
		if persists == 2 {
			return persistErr
		}
		return nil
	}}
	err := gate.Drain(context.Background(), workerupdate.GateRequest{Previous: workerupdate.Release{Version: "1"}})
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.Is(err, persistErr) || errors.As(err, &busy) {
		t.Fatalf("drain=%v", err)
	}
}

func TestUnixInterruptedPreCutoverDrainDoesNotRestartParticipants(t *testing.T) {
	inner := &gateFixture{}
	participants := &participantFixture{probe: UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "ready"}}
	controller := &controllerFixture{}
	gate := &unixParticipantGate{inner: inner, participants: participants, controller: controller, handoff: &unixActivationHandoff{Baseline: participants.probe}, persist: func() error { return nil }}
	if err := gate.Rollback(context.Background(), workerupdate.GateRequest{Previous: workerupdate.Release{Version: "2"}, Candidate: workerupdate.Release{Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	if participants.restarts != 0 || controller.restarts != 1 || inner.rollbacks != 1 {
		t.Fatalf("participants=%d updater=%d rollback=%d", participants.restarts, controller.restarts, inner.rollbacks)
	}
}

func TestUnixHandoffProtectedPersistenceAndExclusiveOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected production state requires root; exercised by bounded root test executable")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := unixActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := unixActivationLock(root); !errors.Is(err, ErrActivationPending) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("concurrent owner accepted: %v", err)
	}
	lock.Close()
	lock, err = unixActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	content := []byte("verified previous release fixture")
	digest := sha256.Sum256(content)
	previous := workerupdate.Release{Version: "1.2.3.4", SHA256: hex.EncodeToString(digest[:]), Length: int64(len(content))}
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: previous, Candidate: "1.2.3.5", Baseline: UnixParticipantProbe{Running: true, State: "ready"}, RequireInventory: true, ActiveTerminalBusy: true, BusyRequiredVersion: "1.2.3.5", BusyNextCheckAt: time.Now().UTC().Add(5 * time.Minute)}
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	loaded, err := readUnixHandoff(root)
	if err != nil || loaded.Candidate != handoff.Candidate || !loaded.RequireInventory || !loaded.ActiveTerminalBusy || loaded.BusyRequiredVersion != handoff.Candidate {
		t.Fatalf("durable handoff=%#v %v", loaded, err)
	}
	binary := filepath.Join(root, "installed")
	if err := os.WriteFile(binary, content, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "activation", "pb")
	if err := verifiedUnixHelper(binary, helper, previous); err != nil {
		t.Fatal(err)
	}
	if err := verifyUnixExecutable(helper, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("tampered previous release fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifiedUnixHelper(binary, helper, previous); !errors.Is(err, workerupdate.ErrInvalidRelease) {
		t.Fatalf("accepted untrusted helper: %v", err)
	}
	if err := verifyUnixExecutable(helper, previous); err != nil {
		t.Fatalf("failed preparation destroyed known helper: %v", err)
	}
	if err := os.Chmod(unixHandoffPath(root), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readUnixHandoff(root); err == nil {
		t.Fatal("accepted unprotected handoff")
	}
	if err := os.Chmod(unixHandoffPath(root), 0600); err != nil {
		t.Fatal(err)
	}
	retirementErr := errors.New("native helper retirement failed")
	if err := retireUnixHandoff(context.Background(), root, &controllerFixture{retireErr: retirementErr}); !errors.Is(err, retirementErr) {
		t.Fatalf("retirement=%v", err)
	}
	if _, err := readUnixHandoff(root); err != nil {
		t.Fatalf("retirement failure lost recovery marker: %v", err)
	}
	if err := verifyUnixExecutable(helper, previous); err != nil {
		t.Fatalf("retirement failure lost recovery executable: %v", err)
	}
	if err := retireUnixHandoff(context.Background(), root, &controllerFixture{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper remains after retirement: %v", err)
	}
}

func TestOrdinaryUnixHandoffOmitsActiveTerminalBlockFields(t *testing.T) {
	body, err := json.Marshal(unixActivationHandoff{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"active_terminal_busy", "busy_required_version", "busy_next_check_at"} {
		if strings.Contains(string(body), `"`+field+`"`) {
			t.Fatalf("ordinary handoff contains %q: %s", field, body)
		}
	}
}
