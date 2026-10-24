package tailnet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/backoff"
)

type NetworkAPI interface {
	RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
	PeerNetworkConfiguration(context.Context, string, []api.AdmittedNetworkScope) (api.PeerNetworkConfigurationResult, error)
}

// Register persists a proposed key before sending its public half. Deterministic
// operation identity makes a lost response retryable after process restart.
// Only a verified configuration commits the staged key.
func (a *Authority) Register(ctx context.Context, client NetworkAPI, rotate bool) error {
	if client == nil || ctx == nil {
		return ErrAuthority
	}
	public, generation, err := a.PrepareKey(rotate)
	if err != nil {
		return err
	}
	// Rebind an unchanged WireGuard key when the endpoint certificate was
	// renewed. The server keeps the network key generation in that case.
	discovery, err := a.discoveryPublicKey(public)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(a.options.Issuer + "\x00" + a.options.Self.AccountID + "\x00" + a.options.Self.EndpointID + "\x00" + a.options.Self.QUICCertificateFingerprint + "\x00" + strconv.FormatUint(generation, 10) + "\x00" + public))
	_, err = client.RegisterPeerNetwork(ctx, api.PeerNetworkRegistration{OperationID: "network_" + hex.EncodeToString(hash[:]), WireGuardPublicKey: public, DiscoPublicKey: discovery, ExpectedKeyGeneration: generation, QUICCertificateFingerprint: a.options.Self.QUICCertificateFingerprint})
	if err != nil {
		return err
	}
	return a.Refresh(ctx, client)
}

func (a *Authority) Refresh(ctx context.Context, client NetworkAPI) error {
	if client == nil || ctx == nil {
		return ErrAuthority
	}
	select {
	case a.refreshSlot <- struct{}{}:
		defer func() { <-a.refreshSlot }()
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return ErrAuthority
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	operation, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	result, err := client.PeerNetworkConfiguration(ctx, "operation_"+operation.String(), a.AdmittedScopes())
	if err != nil {
		var apiErr *api.APIError
		if errors.Is(err, api.ErrUnauthenticated) || errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			a.mu.Lock()
			a.dropLocked()
			a.mu.Unlock()
		}
		return err
	}
	if refresher, ok := a.options.Keys.(interface{ Refresh(context.Context) error }); ok {
		if err := refresher.Refresh(ctx); err != nil {
			return err
		}
	}
	if err := a.Apply(ctx, result.Configuration); err != nil {
		if errors.Is(err, ErrStaleAuthority) {
			return nil
		}
		return err
	}
	a.setRelayDenial(result.RelayDenial())
	if result.CandidateSet == "" {
		a.relay.mu.Lock()
		a.relay.tokens = nil
		a.relay.grants = nil
		a.relay.mu.Unlock()
		return nil
	}
	if err := a.ApplyRegionalCandidates(ctx, result.CandidateSet); err != nil {
		a.relay.mu.Lock()
		a.relay.tokens = nil
		a.relay.grants = nil
		a.relay.mu.Unlock()
		return err
	}
	return a.ApplyRelayGrants(ctx, result.RelayGrants)
}

// Run refreshes current authority with bounded requests. Cancellation releases
// every attached engine. Transient control-plane loss never extends cached expiry.
// Report receives errors only; it must not log configuration or private custody.
func (a *Authority) Run(ctx context.Context, client NetworkAPI, report func(error)) error {
	if ctx == nil || client == nil {
		return ErrAuthority
	}
	defer a.Close()
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-a.done:
			cancelRun()
		case <-watchDone:
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	due := time.Now() // when timer fires
	reset := func(wait time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		due = time.Now().Add(wait)
	}
	// An on-demand request or an applied configuration may bring the next refresh forward, never push it
	// back: the wake from the configuration a refresh had just applied re-armed the regular cadence over a
	// pending on-demand refresh (an announced withdrawal then waited for the next renewal), and earlier
	// over the regional recovery renewal (every relay withdrawn about every 17 s).
	earlier := func(wait time.Duration) {
		if time.Now().Add(wait).Before(due) {
			reset(wait)
		}
	}
	var lastRefresh time.Time
	failures := 0
	for {
		select {
		case <-a.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-a.refreshNow:
			if failures > 0 {
				// The server is failing: the backoff schedule already decides when to retry.
				continue
			}
			earlier(max(0, onDemandRefreshSpacing-time.Since(lastRefresh)))
		case <-a.refreshWake:
			// A configuration applied elsewhere may expire sooner than the current schedule.
			earlier(a.nextRefresh(nil, 0))
		case <-timer.C:
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.Refresh(request, client)
			lastRefresh = time.Now()
			cancel()
			if err != nil {
				failures++
			} else {
				failures = 0
			}
			if err != nil && report != nil {
				report(err)
			}
			if denial := a.RelayDenial(); err == nil && denial != nil && report != nil {
				report(denial)
			}
			reset(a.nextRefresh(err, failures))
		}
	}
}

// nextRefresh is the wait before the next refresh after one that ended with err (nil on success)
// and failures consecutive failures.
func (a *Authority) nextRefresh(err error, failures int) time.Duration {
	delay := regionalRefreshDelay()
	a.relay.mu.Lock()
	recovering := a.relay.recovery != nil
	a.relay.mu.Unlock()
	// Signed health observations expire regionalObservationMaxAge after they were
	// made, not after they were issued. Active regional recovery renews before the
	// earliest one ends, so its relays are not withdrawn between renewals.
	if recovering {
		delay = 8*time.Second + regionalProbeDelay()/2
		if due, ok := a.regionalRenewalDelay(time.Now()); ok {
			delay = due
		}
	}
	// A signed configuration may be much shorter than the regional cadence.
	// Reserve half its remaining lifetime (at most the request bound) for
	// renewal. A failed refresh retries after a short, growing, jittered
	// delay; cached authority still expires independently, including while a
	// request is pending.
	if err != nil {
		delay = min(delay, refreshRetryDelay(failures))
	}
	return a.refreshDelay(delay)
}

// regionalRenewBefore is how long before the earliest signed observation ends a renewal starts;
// regionalMinimumRenewal bounds renewals when the server's observations arrive already old.
const (
	regionalRenewBefore    = 3 * time.Second
	regionalMinimumRenewal = 4 * time.Second
)

// regionalRenewalDelay is when to renew so the eligible relays' observations stay fresh. It
// reports false when no relay is eligible.
func (a *Authority) regionalRenewalDelay(now time.Time) (time.Duration, bool) {
	a.mu.Lock()
	candidates := a.regional
	a.mu.Unlock()
	nodes, _, err := candidates.Eligible("relay", "derp_quic", nil, now)
	if err != nil || len(nodes) == 0 {
		return 0, false
	}
	return regionalRenewalAfter(nodes, now), true
}

func regionalRenewalAfter(nodes []RegionalNode, now time.Time) time.Duration {
	earliest := time.Unix(min(nodes[0].ObservedAt, nodes[0].CapacityObservedAt), 0)
	for _, n := range nodes[1:] {
		if observed := time.Unix(min(n.ObservedAt, n.CapacityObservedAt), 0); observed.Before(earliest) {
			earliest = observed
		}
	}
	return max(earliest.Add(regionalObservationMaxAge-regionalRenewBefore).Sub(now), regionalMinimumRenewal)
}

// refreshRetryDelay is the wait after the failures-th consecutive failed refresh: one second,
// doubling to a one-minute ceiling, each spread over half to one and a half times (backoff.Retry)
// so clients that failed together do not retry together.
func refreshRetryDelay(failures int) time.Duration {
	return backoff.Retry(failures, time.Second, time.Minute)
}

// onDemandRefreshSpacing bounds how often RequestRefresh can make Run call the server.
const onDemandRefreshSpacing = time.Second

func regionalRefreshDelay() time.Duration {
	var random [1]byte
	if _, err := rand.Read(random[:]); err != nil {
		return RefreshInterval
	}
	// The declared ±20% jitter is bounded to whole milliseconds and never
	// changes the 30-second average refresh cadence.
	return 24*time.Second + time.Duration(uint64(random[0])*12_000/255)*time.Millisecond
}

// refreshDelay never extends signed validity and avoids a busy loop when the
// issuer repeatedly returns a configuration at the end of its grant lifetime.
func (a *Authority) refreshDelay(cadence time.Duration) time.Duration {
	if expiry := a.expiresAt.Load(); expiry != 0 {
		remaining := time.Until(time.Unix(expiry, 0))
		margin := remaining / 2
		if margin > 10*time.Second {
			margin = 10 * time.Second
		}
		if due := remaining - margin; due < cadence {
			cadence = due
		}
	}
	if a.expiresAt.Load() == 0 && cadence > time.Second {
		cadence = time.Second
	}
	if cadence < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	return cadence
}
