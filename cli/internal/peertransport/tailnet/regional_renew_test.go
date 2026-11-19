package tailnet

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

func regionalSetFor(c NetworkConfiguration, authorizationGeneration uint64, expires time.Time) RegionalCandidates {
	now := time.Now().Unix()
	return RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: c.Issuer, Audience: "paperboat-regional-candidates", AccountID: c.Self.AccountID, EndpointID: c.Self.EndpointID,
		AuthorizationGeneration: authorizationGeneration, Generation: c.Generation, IssuedAt: now, ExpiresAt: expires.Unix(), Nodes: []RegionalNode{}}
}

// A canary `pb send` failed once (2026-11-19 11:00:07) with "regional candidate authority is invalid or expired", 20 ms
// after a renewal answered: the answer's candidate set was refused and the operation failed. It now asks again.
func TestAdmissionRenewalAsksAgainWhenTheAnsweredSetIsRefused(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c = untilReplaced(c)
	var asked atomic.Int32
	client := relayCounting{answer: func() api.PeerNetworkConfigurationResult {
		set := regionalSetFor(c, c.Generation, time.Now().Add(time.Hour))
		if asked.Add(1) == 1 {
			set.AuthorizationGeneration++ // signed under another generation: refused
		}
		return api.PeerNetworkConfigurationResult{Configuration: networkToken(t, signer, c), CandidateSet: regionalToken(t, signer, set)}
	}}
	if err := a.RefreshForAdmission(t.Context(), client); err != nil {
		t.Fatalf("an admission renewal whose first answer was refused failed: %v", err)
	}
	if asked.Load() != 2 {
		t.Fatalf("%d renewals, want the refused one and one more", asked.Load())
	}
	if _, err := a.RegionalNodes(time.Now()); err != nil {
		t.Fatalf("no candidate set held after the second answer: %v", err)
	}
}

// A set that stays refused is bounded: the operation fails with the check that refused it named.
func TestAdmissionRenewalGivesUpOnASetThatStaysRefused(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c = untilReplaced(c)
	var asked atomic.Int32
	client := relayCounting{answer: func() api.PeerNetworkConfigurationResult {
		asked.Add(1)
		set := regionalSetFor(c, c.Generation, time.Now().Add(-time.Minute)) // expired on arrival
		return api.PeerNetworkConfigurationResult{Configuration: networkToken(t, signer, c), CandidateSet: regionalToken(t, signer, set)}
	}}
	err := a.RefreshForAdmission(t.Context(), client)
	if !errors.Is(err, ErrRegionalAuthority) || err.Error() == ErrRegionalAuthority.Error() {
		t.Fatalf("error %q, want ErrRegionalAuthority naming the failed check", err)
	}
	if asked.Load() != admissionRefusedRetries+1 {
		t.Fatalf("%d renewals, want %d", asked.Load(), admissionRefusedRetries+1)
	}
}

// The renewal of a regional candidate set is due with half of its remaining life left, however long the
// configuration lives, and a host whose timer fires late (its clock jumped on resume) renews at once.
func TestRegionalSetIsRenewedWithHalfItsLifeLeft(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c = untilReplaced(c) // the configuration lives 12 h: only the set bounds the schedule
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, regionalSetFor(c, c.Generation, time.Now().Add(10*time.Minute)))); err != nil {
		t.Fatal(err)
	}
	for range 64 {
		if got := a.nextRefresh(nil, 0, 0); got > 5*time.Minute {
			t.Fatalf("a set with 10 minutes left was renewed after %v, want within half its remaining life", got)
		}
	}
	// The host was frozen for 9 minutes: the server's clock reads 9 minutes ahead of its own.
	sent := time.Now()
	serverclock.Observe(sent.Add(9*time.Minute).UTC().Format(http.TimeFormat), sent, sent.Add(10*time.Millisecond))
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	if got := a.nextRefresh(nil, 0, 0); got > 31*time.Second {
		t.Fatalf("a set with a minute left was renewed after %v, want within 30 s", got)
	}
}

type relayCounting struct {
	answer func() api.PeerNetworkConfigurationResult
}

func (relayCounting) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, nil
}
func (r relayCounting) PeerNetworkConfiguration(context.Context, api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	return r.answer(), nil
}
