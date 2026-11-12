package tailnet

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

type NetworkAPI interface {
	RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
	PeerNetworkConfiguration(context.Context, api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error)
}

// Register persists a proposed key before sending its public half. Deterministic
// operation identity makes a lost response retryable after process restart.
// Only a verified configuration commits the staged key.
func (a *Authority) Register(ctx context.Context, client NetworkAPI, rotate bool) error {
	if err := a.registerKey(ctx, client, rotate); err != nil {
		return err
	}
	return a.Refresh(ctx, client)
}

// registerKey sends the public registration material; the server keeps the network key generation when only the
// endpoint certificate changed.
func (a *Authority) registerKey(ctx context.Context, client NetworkAPI, rotate bool) error {
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
		return a.rejectRefusedAuthority(err)
	}
	return nil
}

// reregisterAfter bounds how often a refused identity is registered again.
const reregisterAfter = 5 * time.Minute

// refusedConfiguration is what a refresh does with a configuration this authority refuses: it names why in the log and to the
// daemon's health, and re-registers when the server holds another identity for this machine than the pinned one
// (a renewed certificate), so every later refresh is not refused for the same reason.
func (a *Authority) refusedConfiguration(ctx context.Context, client NetworkAPI, err error) {
	diagnosticlog.TryWarn("peer network configuration refused", "reason", err)
	if a.options.OnAuthority != nil {
		a.options.OnAuthority(err)
	}
	last := a.reregistered.Load()
	if !errors.Is(err, ErrSelfMismatch) || last != nil && time.Since(*last) < reregisterAfter {
		return
	}
	now := time.Now()
	a.reregistered.Store(&now)
	if regErr := a.registerKey(ctx, client, false); regErr != nil {
		diagnosticlog.TryWarn("registering this machine again failed", "error", regErr)
	}
}

// An explicit denial revokes the restart cache too; an outage never does.
func (a *Authority) rejectRefusedAuthority(err error) error {
	var response *api.APIError
	if !errors.Is(err, api.ErrUnauthenticated) && !(errors.As(err, &response) && (response.Status == 401 || response.Status == 403)) {
		return err
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	cacheErr := a.options.Store.SavePeerNetworkConfiguration(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, "")
	a.mu.Lock()
	a.dropLocked()
	a.mu.Unlock()
	return errors.Join(err, cacheErr)
}

// RestoreOrRegister resumes exact verified authority before doing control-plane work.
// Run renews it immediately; a new identity must register before it can listen.
func (a *Authority) RestoreOrRegister(ctx context.Context, client NetworkAPI) error {
	if ctx == nil || client == nil {
		return ErrAuthority
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.Restore(ctx); err == nil {
		diagnosticlog.TryInfo("native packet lifecycle", "stage", "restore_ok")
		return nil
	}
	diagnosticlog.TryInfo("native packet lifecycle", "stage", "restore_failed")
	return a.Register(ctx, client, false)
}

// RegisterOrRestore registers and refreshes; when Paperboat cannot be reached (no answer, or a 5xx), it
// restores the configuration last verified on this machine instead, so a restart during an outage keeps the
// peers (F21-01). An answer refusing the machine (4xx) is returned: refused authority is never restored.
// Run keeps refreshing either way.
func (a *Authority) RegisterOrRestore(ctx context.Context, client NetworkAPI, rotate bool) error {
	err := a.Register(ctx, client, rotate)
	var answered *api.APIError
	if err == nil || errors.As(err, &answered) && answered.Status < 500 {
		return err
	}
	if restoreErr := a.Restore(context.WithoutCancel(ctx)); restoreErr != nil {
		return errors.Join(err, restoreErr)
	}
	diagnosticlog.TryWarn("Paperboat is unreachable; using the last verified peer network configuration", "error", err)
	a.renewalFailing.Store(true)
	return nil
}

// Refresh renews the signed configuration now. A machine engine waits for a token of the process's renewal
// budget first (bounded by ctx): per-operation refreshes for new grants bypassed it and ran a host at 15 a
// minute under inspect load (hp 2026-10-27).
func (a *Authority) Refresh(ctx context.Context, client NetworkAPI) error {
	if a.budgeted() {
		if err := renewalBudget.wait(ctx, false); err != nil {
			return err
		}
	}
	return a.refresh(ctx, client, time.Time{})
}

// RefreshForAdmission renews before admitting a stream whose server-signed credential names a grant this host does
// not hold yet (a grant just added). Like a server-announced change it never waits for the budget, only for a 429's
// hold: it is one request per new grant, and waiting for a token let admission time out (F9-04, hp 2026-10-27).
//
// Like Tailscale's controlclient updateRoutine, a 429 waits the server's Retry-After and retries within ctx
// instead of refusing the stream, and every admission whose stream arrived before a renewal started shares
// that renewal: a burst of new operations is one request, not one each (pbvm4 feat9: per-stream renewals hit
// the machine's 429, refused a stream and held every admission 12 s).
func (a *Authority) RefreshForAdmission(ctx context.Context, client NetworkAPI) error {
	asked := time.Now()
	for retry := false; ; retry = true {
		if retry || a.budgeted() {
			if err := renewalBudget.wait(ctx, true); err != nil {
				return err
			}
		}
		err := a.refresh(ctx, client, asked)
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			return err
		}
	}
}

// budgeted reports whether this engine's renewals count against the machine's renewal budget: every engine
// on a machine credential. The CLI daemon renews on the user's credential and a user command must not wait.
func (a *Authority) budgeted() bool { return budgetedEngine(a) }

var budgetedEngine = func(a *Authority) bool { return a.options.Self.Role != "cli" }

// refresh renews the signed configuration. A non-zero since is satisfied without a request by a renewal
// that sent its request after since: the server signed that configuration after anything committed before.
func (a *Authority) refresh(ctx context.Context, client NetworkAPI, since time.Time) error {
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
	if !since.IsZero() && a.refreshedFrom.After(since) {
		return nil
	}
	started := time.Now()
	operation, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	request := api.PeerNetworkConfigurationRequest{OperationID: "operation_" + operation.String(), AdmittedScopes: a.AdmittedScopes()}
	request.Endpoints = a.endpointReportDue()
	request.MembershipGrants = a.membershipGrants.Load()
	request.Authority = api.PeerNetworkAuthorityUntilRevoked
	result, err := client.PeerNetworkConfiguration(ctx, request)
	a.renewalFailing.Store(err != nil) // every renewal path (Run, Refresh, admission) says whether Paperboat answered
	diagnosticlog.TryInfo("native packet lifecycle", "stage", "refresh_answered", "failed", err != nil, "duration_ms", time.Since(started).Milliseconds())
	if err == nil && request.Endpoints != nil {
		a.endpointsReported(*request.Endpoints)
	}
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests {
			renewalBudget.hold(time.Now(), apiErr.RetryAfter)
		}
		return a.rejectRefusedAuthority(err)
	}
	if err := a.knowSigningKeys(ctx, append([]string{result.Configuration, result.CandidateSet}, result.RelayGrants...)); err != nil {
		return err
	}
	if err := a.Apply(ctx, result.Configuration); err != nil {
		if errors.Is(err, ErrStaleAuthority) {
			a.refreshedFrom = started
			return nil
		}
		if errors.Is(err, ErrAuthority) {
			a.refusedConfiguration(ctx, client, err)
		}
		return err
	}
	if a.options.OnAuthority != nil {
		a.options.OnAuthority(nil)
	}
	a.refreshedFrom = started
	a.setRelayDenial(result.RelayDenial())
	if a.membershipGrants.Swap(result.MembershipGrants) != result.MembershipGrants && result.MembershipGrants {
		a.RequestRefresh() // ask for membership grants on the next renewal
	}
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
	// The native owner closes QUIC sessions before closing this packet authority.
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
	var lastRefresh, lastUrgent time.Time
	urgent := false // the pending renewal was announced by the server: it skips the budget's wait
	failures := 0
	unusable := 0 // consecutive successful renewals that brought no usable relay
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
		case <-a.refreshUrgent:
			// A server-announced change (revocation, peer change) is never kept waiting for the budget, only
			// spaced, so an event storm stays bounded: a request inside the spacing is deferred to its end
			// (not handed to the budget), and requests while one is pending coalesce into it. A failing server
			// keeps its backoff.
			if failures == 0 && !urgent {
				wait := max(0, urgentRefreshSpacing-time.Since(lastUrgent))
				lastUrgent, urgent = time.Now().Add(wait), true
				earlier(wait)
			}
		case <-a.refreshWake:
			// A configuration applied elsewhere may expire sooner than the current schedule.
			earlier(a.nextRefresh(nil, 0, unusable))
		case <-timer.C:
			if a.budgeted() {
				if wait := renewalBudget.take(time.Now(), urgent); wait > 0 {
					reset(wait)
					continue
				}
			}
			urgent = false
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.refresh(request, client, time.Time{})
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
			if err == nil {
				if _, usable := a.regionalRenewalDelay(serverclock.Now()); usable {
					unusable = 0
				} else {
					unusable++
				}
			}
			reset(a.nextRefresh(err, failures, unusable))
		}
	}
}

// knowSigningKeys fetches the key set only when a token names a key the cache does not hold (a rotation, or
// the cache's own lifetime ending). Every refresh fetched it before, about twice a minute per engine; the
// set now changes on rotation only, and the server says how long a fetched one stays current (F21-18).
func (a *Authority) knowSigningKeys(ctx context.Context, tokens []string) error {
	refresher, ok := a.options.Keys.(interface{ Refresh(context.Context) error })
	if !ok {
		return nil
	}
	for _, token := range tokens {
		encoded, _, found := strings.Cut(token, ".")
		if !found {
			continue
		}
		header, err := base64.RawURLEncoding.DecodeString(encoded)
		var h struct {
			KeyID string `json:"kid"`
		}
		if err != nil || json.Unmarshal(header, &h) != nil || h.KeyID == "" {
			continue // the token's own verification refuses it
		}
		if _, known, err := a.options.Keys.Lookup(ctx, h.KeyID); err == nil && known {
			continue
		}
		return refresher.Refresh(ctx)
	}
	return nil
}

// regionalUnusableRetry and regionalUnusableRetryMaximum space renewals while the signed configuration
// names no usable relay (a relay outage, or relays withheld): every engine renewing every ~9 s, or every
// second before, for as long as that lasted is the whole fleet polling. No topic announces a relay coming
// back, so the cap bounds how long a recovered relay waits to be used.
const (
	regionalUnusableRetry        = 8 * time.Second
	regionalUnusableRetryMaximum = time.Minute
)

// nextRefresh is the wait before the next refresh after one that ended with err (nil on success),
// failures consecutive failures and unusable consecutive renewals that brought no usable relay.
func (a *Authority) nextRefresh(err error, failures, unusable int) time.Duration {
	delay := a.idleRefreshDelay()
	a.relay.mu.Lock()
	recovery := a.relay.recovery
	a.relay.mu.Unlock()
	// Signed health observations expire regionalObservationMaxAge after they were
	// made, not after they were issued. Active regional recovery renews before the
	// earliest one ends, so its relays are not withdrawn between renewals.
	recovering := recovery != nil && (recovery.pair() != "" || recovery.repair.Load())
	switch {
	case recovering:
		delay = backoff.Retry(unusable, regionalUnusableRetry, regionalUnusableRetryMaximum)
		if due, ok := a.regionalRenewalDelay(serverclock.Now()); ok {
			delay = due
		}
	case unusable > 0:
		// No relay is usable and no topic announces one coming back: retry on the regional backoff.
		delay = backoff.Retry(unusable, regionalUnusableRetry, regionalUnusableRetryMaximum)
	}
	// A signed configuration may be much shorter than the regional cadence.
	// Reserve half its remaining lifetime (at most the request bound) for
	// renewal. A failed refresh retries after a short, growing, jittered
	// delay; cached authority still expires independently, including while a
	// request is pending.
	if err != nil {
		delay = min(delay, refreshRetryDelay(failures))
		if a.expiresAt.Load() == 0 {
			// No configuration yet: refreshDelay would clamp to 1 s, so a 429 or an outage before the first
			// configuration retried every second forever (2 req/s per carrier install, prod 2026-10-27).
			return delay
		}
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
	nodes, _, err := eligible(candidates, "relay", "derp_quic", nil, now)
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

// urgentRefreshSpacing bounds the renewals server-announced changes start outside the renewal budget. A revoked
// CLI session must end its running exec and terminal within about 2 s, so a revoke that follows another
// announcement waits at most this long.
const urgentRefreshSpacing = 2 * time.Second

// onDemandRefreshSpacing bounds how often RequestRefresh can make Run call the server.
const onDemandRefreshSpacing = time.Second

// idleRefreshDelay is the fallback renewal of a configuration nobody announced a change to, 15 to 25 minutes
// by default (three quarters to five quarters of the engine's IdleRefresh).
// The signed network stays in force until a newer one or a revocation replaces it (Tailscale's netmap has no
// expiry), every change wakes a renewal through machine events, and an event stream that reconnects wakes one
// too, so this only bounds how long a missed event can leave an endpoint stale.
func (a *Authority) idleRefreshDelay() time.Duration {
	base := cmp.Or(a.options.IdleRefresh, IdleRefresh)
	var random [1]byte
	if _, err := rand.Read(random[:]); err != nil {
		return base
	}
	return base*3/4 + time.Duration(uint64(random[0])*uint64(base/2)/255)
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
		// Past exp (Paperboat unreachable) the configuration stays in force; retry at the normal cadence
		// instead of every 100 ms.
		if due := remaining - margin; due > 0 && due < cadence {
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
