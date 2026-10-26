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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	nativeService "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
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
		// The control plane refusing an out-of-date client (426) looks like this; the update is the cure.
		{"cached inventory behind an unreachable control plane can still be updated", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2, ControlPlaneUnavailableOnly: true}, UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2, ControlPlaneUnavailableOnly: true}, true, true},
		{"unhealthy for a local reason can still be updated", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2}, UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2}, true, true},
		{"unhealthy for a local reason recovers", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2}, ready, true, true},
		{"unhealthy still needs the new daemon version", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2}, UnixParticipantProbe{Running: true, Version: "0", UpdaterVersion: "1", State: "degraded", Machines: 2}, true, false},
		{"unhealthy still needs a running daemon", UnixParticipantProbe{Running: true, Version: "1", UpdaterVersion: "1", State: "degraded", Machines: 2}, UnixParticipantProbe{UpdaterVersion: "1"}, true, false},
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
	open     liveconnections.Counts
	openErr  error
}

func (p *participantFixture) OpenConnections(context.Context) (liveconnections.Counts, error) {
	return p.open, p.openErr
}

func (p *participantFixture) Probe(context.Context) (UnixParticipantProbe, error) {
	return p.probe, p.probeErr
}
func (p *participantFixture) Restart(context.Context) error { p.restarts++; return nil }

type controllerFixture struct {
	retireErr error
}

func (c *controllerFixture) Install(context.Context, string, map[string]string) error { return nil }
func (c *controllerFixture) Retire(context.Context) error                             { return c.retireErr }

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
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, ApprovalID: strings.Repeat("a", 64), Previous: previous, Candidate: "1.2.3.5", Baseline: UnixParticipantProbe{Running: true, State: "ready"}, RequireInventory: true, ActiveTerminalBusy: true, BusyRequiredVersion: "1.2.3.5", BusyNextCheckAt: time.Now().UTC().Add(5 * time.Minute)}
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

func TestDeferredManualUpdateSurvivesRestartAndAutomaticRetry(t *testing.T) {
	journal := updateflow.Journal{BlockedReason: autoupdate.BlockedActiveTerminalSessions, RequiredVersion: "15", DeferredManual: true}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	journal = updateflow.Journal{}
	if err = json.Unmarshal(encoded, &journal); err != nil {
		t.Fatal(err)
	}
	automaticCalls, manualCalls := 0, 0
	automatic := func(context.Context) (workerupdate.Release, bool, error) {
		automaticCalls++
		return workerupdate.Release{}, false, nil
	}
	explicit := func(context.Context) (workerupdate.Release, bool, error) {
		manualCalls++
		return workerupdate.Release{Version: "15"}, true, nil
	}
	now := time.Now().UTC()
	busy := true
	scheduler, err := autoupdate.New(autoupdate.Config{Now: func() time.Time { return now }, Check: func(ctx context.Context) (autoupdate.Result, error) {
		release, found, manual, e := resolveQueuedRelease(ctx, false, journal, automatic, explicit)
		if e != nil {
			return autoupdate.Result{}, e
		}
		if !found || !manual {
			return autoupdate.Result{}, errors.New("manual retry intent lost")
		}
		if busy {
			return autoupdate.Result{Version: "14"}, &autoupdate.ActiveTerminalSessionsError{RequiredVersion: release.Version}
		}
		return autoupdate.Result{Version: release.Version, Updated: true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.SeedBlockedActiveTerminalSessions("15", now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err = scheduler.CheckNow(context.Background())
		var blocked *autoupdate.ActiveTerminalSessionsError
		if !errors.As(err, &blocked) || scheduler.Snapshot().RequiredVersion != "15" {
			t.Fatal("busy intent lost")
		}
		now = now.Add(autoupdate.DefaultRetryFloor)
	}
	busy = false
	if _, err = scheduler.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := scheduler.Snapshot(); state.RequiredVersion != "" || state.BlockedReason != "" || !state.Updated {
		t.Fatalf("activation did not clear intent: %+v", state)
	}
	if automaticCalls != 0 || manualCalls != 3 {
		t.Fatalf("resolver calls automatic=%d manual=%d", automaticCalls, manualCalls)
	}
	journal.DeferredManual = false
	_, found, manual, err := resolveQueuedRelease(context.Background(), false, journal, automatic, explicit)
	if err != nil || found || manual || automaticCalls != 1 {
		t.Fatal("automatic request bypassed cohort")
	}
}

func TestDeferredManualUpdateStillRequiresCurrentExactSignedRelease(t *testing.T) {
	journal := updateflow.Journal{BlockedReason: autoupdate.BlockedActiveTerminalSessions, RequiredVersion: "15", DeferredManual: true}
	for _, candidate := range []string{"", "16"} {
		resolver := func(context.Context) (workerupdate.Release, bool, error) {
			return workerupdate.Release{Version: candidate}, candidate != "", nil
		}
		if _, _, _, err := resolveQueuedRelease(context.Background(), false, journal, resolver, resolver); err == nil {
			t.Fatal("missing/replaced signed release accepted")
		}
	}
}

func TestUnixQueuedForcePromotionPreservesDurableIntent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned activation state")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := unixActivationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	approval := strings.Repeat("a", 64)
	h := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: workerupdate.Release{Version: "1.2.3.4", Length: 1, SHA256: strings.Repeat("c", 64)}, Candidate: "1.2.3.5", ApprovalID: approval}
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	if err := authorizeUnixHandoffForce(root, h, strings.Repeat("b", 64), true); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("different candidate: %v", err)
	}
	if err := authorizeUnixHandoffForce(root, h, approval, true); err != nil {
		t.Fatal(err)
	}
	persisted, err := readUnixHandoff(root)
	if err != nil || !persisted.Force || !persisted.Manual {
		t.Fatalf("promotion=%+v err=%v", persisted, err)
	}
	if err := authorizeUnixHandoffForce(root, h, approval, false); err != nil {
		t.Fatal(err)
	}
	persisted, err = readUnixHandoff(root)
	if err != nil || !persisted.Force {
		t.Fatalf("ordinary retry cleared grant: %+v %v", persisted, err)
	}
	h.Force = false
	h.Started = true
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	if err := authorizeUnixHandoffForce(root, h, approval, true); !errors.Is(err, ErrActivationPending) {
		t.Fatalf("started intent escalated: %v", err)
	}
	persisted, err = readUnixHandoff(root)
	if err != nil || persisted.Force {
		t.Fatalf("started intent changed: %+v %v", persisted, err)
	}
}

func TestLegacyRollbackRequiresDurableExactPriorSource(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected handoff requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	previous := workerupdate.Release{Version: "1.2.3.4", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: strings.Repeat("a", 64), Length: 42}
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, ApprovalID: strings.Repeat("a", 64), Previous: previous, Candidate: "1.2.3.5", OwnerProvenanceRecorded: true}
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	legacy, err := UnixLegacyRollback(root, previous)
	if err != nil || !legacy {
		t.Fatalf("original rollback=%v %v", legacy, err)
	}
	changed := previous
	changed.SHA256 = strings.Repeat("b", 64)
	legacy, err = UnixLegacyRollback(root, changed)
	if err != nil || legacy {
		t.Fatalf("changed bytes accepted=%v %v", legacy, err)
	}
	handoff.PriorOwnerRecorded = true
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	legacy, err = UnixLegacyRollback(root, previous)
	if err != nil || legacy {
		t.Fatalf("independent prior accepted as legacy=%v %v", legacy, err)
	}
	handoff.PriorOwnerRecorded = false
	handoff.OwnerProvenanceRecorded = false
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	legacy, err = UnixLegacyRollback(root, previous)
	if err != nil || legacy {
		t.Fatalf("unproven prior accepted=%v %v", legacy, err)
	}
}

// The routing daemon is independent of hostd/updater. Native byte replacement
// alone must not satisfy readiness, and signed-health failure must put that
// daemon back on the verified old bytes after native restoration.
func TestPreparedNativeActivationUpdatesAndRestoresRoutingDaemon(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected activation custody")
	}
	for _, mode := range []string{"healthy", "before native validation", "health rejected", "canceled health", "native recovery pending", "native roles recovery pending"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			oldImage, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			newImage := append(append([]byte(nil), oldImage...), 1)
			binary := filepath.Join(root, "pb")
			if err = os.WriteFile(binary, oldImage, 0700); err != nil {
				t.Fatal(err)
			}
			previous, err := installsource.Inspect(binary, "2026.10.13.1", installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(binary, newImage, 0700); err != nil {
				t.Fatal(err)
			}
			next, err := installsource.Inspect(binary, "2026.10.13.2", installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(binary, oldImage, 0700); err != nil {
				t.Fatal(err)
			}
			oldRelease, err := workerupdate.InstalledBaseline(previous, binary)
			if err != nil {
				t.Fatal(err)
			}
			baseline := UnixParticipantProbe{Running: true, Version: previous.Version, UpdaterVersion: previous.Version, State: "ready", Machines: 1}
			handoff := &unixActivationHandoff{Schema: unixHandoffSchema, Previous: oldRelease, Candidate: next.Version, ApprovalID: strings.Repeat("a", 64), Baseline: baseline, RequireInventory: true, RecoveryTimeout: time.Second, Started: true, ParticipantsTouched: true}
			if err = writeUnixHandoff(root, handoff); err != nil {
				t.Fatal(err)
			}
			routing := &preparedRoutingFixture{binary: binary, old: previous, next: next, process: previous.Version}
			installed := previous
			failure := errors.New("signed health rejected")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := &Service{config: Config{StateRoot: root, Binary: binary, Active: oldRelease, Participants: routing, LoadInstalledSource: func() (installsource.Source, error) { return installed, nil }}}
			service.config.ApplyPrepared = func(nativeCtx context.Context, candidate workerupdate.PreparedActivation) (installsource.Source, error) {
				if err := candidate.Prepare(nativeCtx); err != nil {
					return installsource.Source{}, err
				}
				if mode == "before native validation" {
					return installsource.Source{}, failure
				}
				if err := os.WriteFile(binary, newImage, 0700); err != nil {
					t.Fatal(err)
				}
				err := candidate.Validate(nativeCtx)
				if err != nil {
					if mode != "native recovery pending" {
						if err := os.WriteFile(binary, oldImage, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if mode == "native roles recovery pending" {
						err = errors.Join(err, nativeService.ErrLifecycleUncertain)
					}
					return installsource.Source{}, err
				}
				installed = next
				return next, nil
			}
			committed, err := service.applyPrepared(ctx, workerupdate.PreparedActivation{Source: next, Prepare: func(context.Context) error { return nil }, Validate: func(context.Context) error {
				if routing.process != next.Version {
					t.Fatal("signed health observed old routing daemon")
				}
				if installed != previous {
					t.Fatal("source committed before signed health")
				}
				if mode == "canceled health" {
					cancel()
					return context.Canceled
				}
				if mode != "healthy" {
					return failure
				}
				return nil
			}})
			if mode == "healthy" {
				if err != nil || committed != next || routing.process != next.Version {
					t.Fatal("native success left old routing daemon", err)
				}
				if err = next.Verify(binary); err != nil {
					t.Fatal(err)
				}
				return
			}
			if mode == "canceled health" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation cause lost", err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatal("health failure cause lost", err)
			}
			if mode == "native recovery pending" || mode == "native roles recovery pending" {
				if !errors.Is(err, workerupdate.ErrBlocked) || routing.process != next.Version || committed != (installsource.Source{}) {
					t.Fatal("unverified native recovery restarted old routing daemon", err)
				}
				return
			}
			if committed != previous {
				t.Fatal("verified completed restoration omitted prior source proof", err)
			}
			if routing.process != previous.Version || installed != previous {
				t.Fatal("routing compensation failed", err)
			}
			if err = previous.Verify(binary); err != nil {
				t.Fatal("native fixture did not restore verified old bytes", err)
			}
		})
	}
}

func (p *preparedRoutingFixture) OpenConnections(context.Context) (liveconnections.Counts, error) {
	return liveconnections.Counts{}, nil
}

type preparedRoutingFixture struct {
	binary    string
	old, next installsource.Source
	process   string
}

func (p *preparedRoutingFixture) executableVersion() (string, error) {
	if p.old.Verify(p.binary) == nil {
		return p.old.Version, nil
	}
	if p.next.Verify(p.binary) == nil {
		return p.next.Version, nil
	}
	return "", workerupdate.ErrInvalidRelease
}
func (p *preparedRoutingFixture) Restart(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version, err := p.executableVersion()
	if err != nil {
		return err
	}
	p.process = version
	return nil
}
func (p *preparedRoutingFixture) Probe(ctx context.Context) (UnixParticipantProbe, error) {
	if err := ctx.Err(); err != nil {
		return UnixParticipantProbe{}, err
	}
	version, err := p.executableVersion()
	if err != nil {
		return UnixParticipantProbe{}, err
	}
	return UnixParticipantProbe{Running: true, Version: p.process, UpdaterVersion: version, State: "ready", Machines: 1}, nil
}

func TestUnixNativeRecoveryKeepsApprovalWhileQueuedWorkRechecksLatest(t *testing.T) {
	original := workerupdate.Release{Version: "2026.10.14.1", SHA256: strings.Repeat("a", 64), Length: 1, Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	receipt, err := workerupdate.PreparedCandidateForRelease(original)
	if err != nil {
		t.Fatal(err)
	}
	newer := original
	newer.Version = "2026.10.14.2"
	revoked := errors.New("signed policy revoked")
	for _, tc := range []struct {
		name   string
		proof  bool
		denied string
		want   error
	}{
		{"queued started is not native proof", false, "", ErrCandidateChanged},
		{"protected native receipt resumes original", true, "", nil},
		{"candidate revoked", true, original.Version, revoked},
		{"previous revoked", true, "2026.10.13.1", revoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &unixActivationHandoff{ApprovalID: receipt.ID, Candidate: original.Version, Started: true, ParticipantsTouched: true, Previous: original}
			h.Previous.Version = "2026.10.13.1"
			resolutions := 0
			var authorized []string
			got, err := resolveUnixActivationRelease(context.Background(), h, original, tc.proof, func(context.Context) (workerupdate.Release, bool, error) { resolutions++; return newer, true, nil }, func(_ context.Context, v, p, a string) error {
				authorized = append(authorized, v)
				if v == tc.denied {
					return revoked
				}
				return nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("recovery=%v want=%v", err, tc.want)
			}
			if tc.proof && resolutions != 0 {
				t.Fatal("native recovery substituted latest candidate")
			}
			if !tc.proof && resolutions != 1 {
				t.Fatal("queued approval skipped current eligibility")
			}
			if tc.want == nil && (got.Version != original.Version || len(authorized) != 2 || authorized[0] != original.Version || authorized[1] != h.Previous.Version) {
				t.Fatal("recovery lost original release or fresh rollback authorization")
			}
		})
	}
}

func TestPreparedNativeActivationStrengthensBaselineBeforeDrain(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native handoff requires root")
	}
	for _, mode := range []string{"ready strengthens offline", "running strengthens stopped", "degraded established baseline refused", "committed retry skips prepare"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			image, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(root, "pb")
			if err := os.WriteFile(binary, image, 0700); err != nil {
				t.Fatal(err)
			}
			previous, err := installsource.Inspect(binary, "2026.10.13.1", installsource.Official)
			if err != nil {
				t.Fatal(err)
			}
			release, err := workerupdate.InstalledBaseline(previous, binary)
			if err != nil {
				t.Fatal(err)
			}
			offline := UnixParticipantProbe{Running: true, Version: previous.Version, UpdaterVersion: previous.Version, State: "degraded", ControlPlaneUnavailableOnly: true}
			ready := UnixParticipantProbe{Running: true, Version: previous.Version, UpdaterVersion: previous.Version, State: "ready", Machines: 2}
			baseline, probe := offline, ready
			inventory := false
			if mode == "running strengthens stopped" {
				baseline = UnixParticipantProbe{UpdaterVersion: previous.Version}
			}
			if mode == "degraded established baseline refused" || mode == "committed retry skips prepare" {
				baseline, probe, inventory = ready, offline, true
			}
			h := &unixActivationHandoff{Schema: unixHandoffSchema, ApprovalID: strings.Repeat("a", 64), Previous: release, Candidate: "2026.10.13.2", Baseline: baseline, RequireInventory: inventory}
			if err := writeUnixHandoff(root, h); err != nil {
				t.Fatal(err)
			}
			prepares := 0
			service := &Service{config: Config{StateRoot: root, Binary: binary, Participants: &participantFixture{probe: probe}, LoadInstalledSource: func() (installsource.Source, error) { return previous, nil }}}
			next := previous
			next.Version = h.Candidate
			service.config.ApplyPrepared = func(ctx context.Context, c workerupdate.PreparedActivation) (installsource.Source, error) {
				if mode == "committed retry skips prepare" {
					return next, nil
				}
				if err := c.Prepare(ctx); err != nil {
					return installsource.Source{}, err
				}
				return next, nil
			}
			_, err = service.applyPrepared(context.Background(), workerupdate.PreparedActivation{Source: next, Prepare: func(context.Context) error {
				persisted, err := readUnixHandoff(root)
				if err != nil {
					t.Fatal(err)
				}
				if !persisted.Baseline.Running || !persisted.RequireInventory {
					t.Fatal("drain began before stronger baseline was durable")
				}
				prepares++
				return nil
			}, Validate: func(context.Context) error { return nil }})
			if mode == "degraded established baseline refused" {
				if !errors.Is(err, ErrParticipantReadiness) || prepares != 0 {
					t.Fatalf("degraded pre-drain=%v prepares=%d", err, prepares)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if mode == "committed retry skips prepare" {
				if prepares != 0 {
					t.Fatal("committed retry redrained participants")
				}
			} else if prepares != 1 {
				t.Fatal("native prepare was not called once")
			}
		})
	}
}

// An installation interrupted before its commit that native recovery restored to the previous
// version ends with the routing daemon restarted and ready on that version (luf26a).
func TestRecoveredPreviousInstallationRestartsTheDaemonOnIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected native handoff requires root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous, err := installsource.Inspect(executable, "2026.10.25.1", installsource.Official)
	if err != nil {
		t.Fatal(err)
	}
	release, err := workerupdate.InstalledBaseline(previous, executable)
	if err != nil {
		t.Fatal(err)
	}
	ready := UnixParticipantProbe{Running: true, Version: previous.Version, UpdaterVersion: previous.Version, State: "ready"}
	h := &unixActivationHandoff{Schema: unixHandoffSchema, ApprovalID: strings.Repeat("a", 64), Previous: release, Candidate: "2026.10.26.0", Baseline: ready}
	if err := writeUnixHandoff(root, h); err != nil {
		t.Fatal(err)
	}
	participants := &participantFixture{probe: ready}
	service := &Service{config: Config{StateRoot: root, Participants: participants, RecoverPrepared: func(context.Context) (installsource.Source, error) { return previous, nil }}}
	committed, err := service.recoverPrepared(context.Background())
	if err != nil || committed != previous || participants.restarts != 1 {
		t.Fatalf("restored installation: %+v %v restarts=%d", committed, err, participants.restarts)
	}
	candidate := previous
	candidate.Version = h.Candidate
	service.config.RecoverPrepared = func(context.Context) (installsource.Source, error) { return candidate, nil }
	if committed, err = service.recoverPrepared(context.Background()); err != nil || committed != candidate || participants.restarts != 1 {
		t.Fatalf("committed candidate must not restart the daemon: %+v %v restarts=%d", committed, err, participants.restarts)
	}
}
