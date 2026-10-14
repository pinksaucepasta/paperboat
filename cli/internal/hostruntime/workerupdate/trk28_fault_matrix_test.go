//go:build darwin || linux

package workerupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

func TestTRK28SignedMetadataFaultsFailClosed(t *testing.T) {
	fixture := newFixture(t)
	valid := fixture.candidate
	valid.ManifestSHA256 = strings.Repeat("a", 64)
	valid.CanaryPath = "/healthz"
	valid.CanaryStatus = 200
	valid.CanarySamples = 2
	valid.CanaryTimeout = time.Second
	valid.DrainTimeout = time.Second
	valid.StabilityWindow = 2 * time.Second
	valid.StabilityInterval = time.Second
	valid.RollbackTimeout = time.Second
	if err := ValidateActivationRelease(valid); err != nil {
		t.Fatalf("valid signed release rejected: %v", err)
	}

	tests := []struct {
		name       string
		mutate     func(*Release)
		activation bool
	}{
		{name: "version", mutate: func(r *Release) { r.Version = "2026.8.31" }},
		{name: "artifact_hash", mutate: func(r *Release) { r.SHA256 = strings.Repeat("g", 64) }},
		{name: "artifact_length", mutate: func(r *Release) { r.Length = 0 }},
		{name: "platform", mutate: func(r *Release) { r.Platform = "unknown" }},
		{name: "architecture", mutate: func(r *Release) { r.Architecture = "386" }},
		{name: "hostd_api", mutate: func(r *Release) { r.HostdAPIMin = 0 }},
		{name: "manifest_hash", activation: true, mutate: func(r *Release) { r.ManifestSHA256 = strings.Repeat("A", 64) }},
		{name: "canary_path", activation: true, mutate: func(r *Release) { r.CanaryPath = "healthz" }},
		{name: "canary_samples", activation: true, mutate: func(r *Release) { r.CanarySamples = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if test.activation {
				if !errors.Is(ValidateActivationRelease(candidate), ErrInvalidDeploymentGate) {
					t.Fatalf("invalid signed policy accepted: %+v", candidate)
				}
				return
			}
			if !errors.Is(validateRelease(candidate), ErrInvalidRelease) {
				t.Fatalf("invalid release metadata accepted: %+v", candidate)
			}
		})
	}
}

func TestTRK28ArtifactTruncationAndExtensionNeverStartCandidate(t *testing.T) {
	for _, test := range []struct {
		name string
		body func([]byte) []byte
	}{
		{name: "truncated", body: func(body []byte) []byte { return body[:len(body)-1] }},
		{name: "extended", body: func(body []byte) []byte { return append(append([]byte(nil), body...), 0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			fixture.fetcher.body = test.body(fixture.fetcher.body)
			_, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
			if !errors.Is(err, ErrInvalidRelease) {
				t.Fatalf("artifact fault error=%v", err)
			}
			if *fixture.nativeCalls != 0 || fixture.hostd.activations != 0 {
				t.Fatalf("candidate reached execution: starts=%d activations=%d", *fixture.nativeCalls, fixture.hostd.activations)
			}
			if !regularMatches(fixture.paths.current, fixture.active.Length, fixture.active.SHA256) {
				t.Fatal("active runtime changed after artifact fault")
			}
		})
	}
}

func TestTRK28JournalWriteFailureLeavesTransactionBeforeCandidateStart(t *testing.T) {
	fixture := newFixture(t)
	if err := fixture.manager.write(fixture.manager.newJournal()); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("journal write failed")
	writes := 0
	fixture.manager.config.WriteJournal = func(string, updateflow.Journal, int, int) error {
		writes++
		return writeErr
	}
	_, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate)
	if !errors.Is(err, writeErr) || writes != 1 {
		t.Fatalf("error=%v writes=%d", err, writes)
	}
	if *fixture.nativeCalls != 0 || fixture.hostd.activations != 0 || fixture.manager.ActiveVersion() != fixture.active.Version {
		t.Fatalf("write failure crossed execution boundary: starts=%d activations=%d active=%q", *fixture.nativeCalls, fixture.hostd.activations, fixture.manager.ActiveVersion())
	}
	journal, loadErr := updateflow.Load(fixture.paths.journal)
	if loadErr != nil || journal.Stage != updateflow.StageIdle || journal.ActiveVersion != fixture.active.Version {
		t.Fatalf("durable state=%+v err=%v", journal, loadErr)
	}
}

func TestTRK28ExactGenerationFenceRejectsZeroValues(t *testing.T) {
	base := deploymentTarget()
	toWire := func(target DeploymentTarget) hostdproto.UpdateGateTargetBinding {
		return hostdproto.UpdateGateTargetBinding{Scope: target.Scope, MachineID: target.MachineID, AccountID: target.AccountID, HostID: target.HostID, TunnelID: target.TunnelID, ConnectorID: target.ConnectorID, EdgeNodeID: target.EdgeNodeID, ProcessEpoch: target.ProcessEpoch, SessionGeneration: target.SessionGeneration, ConfigGeneration: target.ConfigGeneration, RouteGeneration: target.RouteGeneration, FailureDomain: target.FailureDomain}
	}
	valid := toWire(base)
	request := hostdproto.UpdateGateRequest{Operation: hostdproto.UpdateGateCommit, TransactionID: "transaction_1", Version: "2026.08.31.1", ManifestSHA256: strings.Repeat("a", 64), ExpectedTarget: &valid}
	if err := validateDeploymentTarget(base); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid signed target rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*DeploymentTarget)
	}{
		{name: "process_epoch", mutate: func(target *DeploymentTarget) { target.ProcessEpoch = 0 }},
		{name: "session_generation", mutate: func(target *DeploymentTarget) { target.SessionGeneration = 0 }},
		{name: "config_generation", mutate: func(target *DeploymentTarget) { target.ConfigGeneration = 0 }},
		{name: "route_generation", mutate: func(target *DeploymentTarget) { target.RouteGeneration = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := base
			test.mutate(&target)
			if err := validateDeploymentTarget(target); err == nil {
				t.Fatal("zero generation accepted by deployment gate")
			}
			wireTarget := toWire(target)
			candidate := request
			candidate.ExpectedTarget = &wireTarget
			if err := candidate.Validate(); err == nil {
				t.Fatal("zero generation accepted by hostd protocol")
			}
		})
	}
}

func TestTRK28RollbackQuarantineDoesNotFreezeNewerRelease(t *testing.T) {
	fixture := newFixture(t)
	fixture.health.err = errors.New("edge health failed")
	if _, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate); err == nil {
		t.Fatal("failed activation unexpectedly succeeded")
	}
	if _, err := activatePrepared(context.Background(), fixture.manager, fixture.candidate); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined exact retry error=%v", err)
	}
	fixture.health.err = nil
	newer := release("2026.08.18.3", fixture.fetcher.body)
	result, err := activatePrepared(context.Background(), fixture.manager, newer)
	if err != nil || !result.Updated || fixture.manager.ActiveVersion() != newer.Version {
		t.Fatalf("newer release was frozen by quarantine: result=%+v err=%v active=%q", result, err, fixture.manager.ActiveVersion())
	}
}

func TestTRK28DrainDeadlineIsBoundedAndDowngradeIsRejected(t *testing.T) {
	fixture := newFixture(t)
	gate := &scriptedGate{drainErr: context.DeadlineExceeded}
	fixture.manager.config.Gate = gate
	fixture.manager.config.DrainTimeout = 2 * time.Millisecond
	candidate, err := fixture.manager.Prepare(context.Background(), fixture.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Approve(context.Background(), candidate.ID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = fixture.manager.Activate(context.Background(), fixture.candidate)
	if !errors.Is(err, context.DeadlineExceeded) || gate.drain != 1 || gate.rollback != 1 || fixture.hostd.activations != 0 {
		t.Fatalf("drain result error=%v gate=%+v activations=%d", err, gate, fixture.hostd.activations)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("drain exceeded bounded recovery window: %s", elapsed)
	}

	downgrade := release("2026.08.17.99", fixture.fetcher.body)
	result, err := activatePrepared(context.Background(), fixture.manager, downgrade)
	if !errors.Is(err, ErrInvalidRelease) || result.Updated || *fixture.nativeCalls != 1 {
		t.Fatalf("downgrade result=%+v err=%v starts=%d", result, err, *fixture.nativeCalls)
	}
}

func TestTRK28DeferralAndPausedPlanFreezeAutomaticSelection(t *testing.T) {
	index, now := validEligibilityIndex(t)
	deferral, err := index.DeploymentPlan.GrantDeferral(releasepolicy.DeferralRequest{Version: index.Version, RequestedSecs: 3600, Reason: "maintenance"}, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	source := TUFSource{MachineID: "machine_1", Deferral: DeferralSourceFunc(func(context.Context) (releasepolicy.Deferral, bool, error) { return deferral, true, nil }), FailureDomain: FailureDomainSourceFunc(func(context.Context, FailureDomainRequest) (string, error) { return "iad-1", nil })}
	eligible, err := source.eligible(context.Background(), index, now, true)
	if err != nil || eligible {
		t.Fatalf("active signed deferral did not freeze selection: eligible=%t err=%v", eligible, err)
	}

	paused := index
	plan := *index.DeploymentPlan
	plan.RolloutState = releasepolicy.RolloutStatePaused
	paused.DeploymentPlan = &plan
	paused.DeploymentPlanSHA256, err = plan.PlanSHA256()
	if err != nil {
		t.Fatal(err)
	}
	source.Deferral = DeferralSourceFunc(func(context.Context) (releasepolicy.Deferral, bool, error) {
		return releasepolicy.Deferral{}, false, nil
	})
	eligible, err = source.eligible(context.Background(), paused, now, true)
	if err != nil || eligible {
		t.Fatalf("paused signed plan did not freeze selection: eligible=%t err=%v", eligible, err)
	}
}
