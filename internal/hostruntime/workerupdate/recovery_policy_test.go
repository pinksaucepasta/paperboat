package workerupdate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

type recoveryPolicyClient struct {
	rollback  hostdproto.UpdateGateRequest
	remaining time.Duration
}

func (c *recoveryPolicyClient) UpdateGate(ctx context.Context, r hostdproto.UpdateGateRequest) (hostdproto.UpdateGateResponse, error) {
	if err := r.Validate(); err != nil {
		return hostdproto.UpdateGateResponse{}, err
	}
	if r.Operation == hostdproto.UpdateGateRollback {
		c.rollback = r
		deadline, ok := ctx.Deadline()
		if !ok {
			return hostdproto.UpdateGateResponse{}, errors.New("missing recovery deadline")
		}
		c.remaining = time.Until(deadline)
	}
	return hostdproto.UpdateGateResponse{Target: hostdproto.UpdateGateTargetBinding{Scope: hostdproto.UpdateGateScopeStandalone, MachineID: "machine_1", FailureDomain: "standalone"}}, nil
}

func TestRecoveryPreservesSignedCandidatePolicy(t *testing.T) {
	f := newFixture(t)
	candidate := f.candidate
	candidate.ManifestSHA256 = strings.Repeat("a", 64)
	candidate.CanaryPath, candidate.CanaryStatus, candidate.CanarySamples = "/signed-recovery", 204, 3
	candidate.CanaryTimeout, candidate.DrainTimeout = 7*time.Second, 9*time.Second
	candidate.StabilityWindow, candidate.StabilityInterval, candidate.RollbackTimeout = 11*time.Second, 2*time.Second, 3*time.Second
	if err := os.WriteFile(f.paths.staged, []byte("candidate"), 0700); err != nil {
		t.Fatal(err)
	}
	j := withRelease(f.manager.newJournal(), candidate, f.paths.staged)
	j.Stage = updateflow.StageDraining
	if err := f.manager.write(j); err != nil {
		t.Fatal(err)
	}
	persisted, err := updateflow.Load(f.paths.journal)
	if err != nil {
		t.Fatal(err)
	}
	restored := releaseFromJournal(persisted)
	client := &recoveryPolicyClient{}
	gate, err := NewDeploymentActivationGate(DeploymentActivationGateConfig{Provider: HostdDeploymentProvider{Client: client}})
	if err != nil {
		t.Fatal(err)
	}
	f.manager.config.Gate = gate
	if err := f.manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if activationPolicy(restored) != activationPolicy(candidate) {
		t.Fatal("signed policy changed across journal reload")
	}
	r := client.rollback
	if r.Operation != hostdproto.UpdateGateRollback || r.Version != candidate.Version || r.PreviousVersion != f.manager.active.Version || r.Path != candidate.CanaryPath || r.ExpectedStatus != 204 || r.Samples != 3 || r.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("recovery lost signed binding: %+v", r)
	}
	if client.remaining <= 0 || client.remaining > candidate.RollbackTimeout || r.TimeoutMillis <= 0 || r.TimeoutMillis > candidate.RollbackTimeout.Milliseconds() {
		t.Fatalf("recovery deadline=%s request=%dms", client.remaining, r.TimeoutMillis)
	}
}

func TestSignedCandidateJournalRejectsMissingOrMalformedPolicy(t *testing.T) {
	f := newFixture(t)
	r := f.candidate
	r.ManifestSHA256 = strings.Repeat("a", 64)
	r.CanaryPath, r.CanaryStatus, r.CanarySamples = "/healthz", 200, 2
	r.CanaryTimeout, r.DrainTimeout, r.StabilityWindow, r.StabilityInterval, r.RollbackTimeout = time.Second, time.Second, time.Second, time.Second, time.Second
	j := withRelease(f.manager.newJournal(), r, f.paths.staged)
	j.Stage = updateflow.StageDraining
	for _, test := range []struct {
		name   string
		mutate func(*updateflow.Journal)
	}{
		{"missing", func(j *updateflow.Journal) { j.CandidatePolicy = nil }},
		{"missing_timeout", func(j *updateflow.Journal) { j.CandidatePolicy.RollbackTimeout = 0 }},
		{"invalid_path", func(j *updateflow.Journal) { j.CandidatePolicy.CanaryPath = "https://elsewhere.test/healthz" }},
		{"too_few_samples", func(j *updateflow.Journal) { j.CandidatePolicy.CanarySamples = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := j
			policy := *j.CandidatePolicy
			copy.CandidatePolicy = &policy
			test.mutate(&copy)
			if !errors.Is(copy.Validate(), updateflow.ErrInvalidJournal) {
				t.Fatal("invalid signed policy accepted")
			}
		})
	}
}
