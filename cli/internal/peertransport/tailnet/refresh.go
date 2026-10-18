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

	"github.com/pinksaucepasta/paperboat-relay/admission"
	"github.com/pinksaucepasta/paperboat/internal/api"
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
	// Live carrier and operation refs need current policy independently of the
	// credential renewal schedule. The shared owner bounds every lookup and
	// keeps accepted work through transient control-plane failures.
	revocations, err := admission.Admit(ctx, time.Time{}, nil)
	if err != nil {
		return err
	}
	defer revocations.Close()
	revocations.Poll(func(check context.Context) error {
		if len(a.AdmittedScopes()) == 0 {
			return nil
		}
		err := a.Refresh(check, client)
		if err != nil && report != nil {
			report(err)
		}
		if denial := a.RelayDenial(); err == nil && denial != nil && report != nil {
			report(denial)
		}
		return err
	})
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
	for {
		select {
		case <-a.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-a.refreshWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(a.refreshDelay(regionalRefreshDelay()))
		case <-timer.C:
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.Refresh(request, client)
			cancel()
			if err != nil && report != nil {
				report(err)
			}
			if denial := a.RelayDenial(); err == nil && denial != nil && report != nil {
				report(denial)
			}
			delay := regionalRefreshDelay()
			a.relay.mu.Lock()
			recovering := a.relay.recovery != nil
			a.relay.mu.Unlock()
			// Signed health observations expire after 15 seconds. Active regional
			// recovery refreshes before that boundary, including bounded jitter.
			if recovering {
				delay = 8*time.Second + regionalProbeDelay()/2
			}
			// A signed configuration may be much shorter than the regional cadence.
			// Reserve half its remaining lifetime (at most the request bound) for
			// renewal. Failed refreshes retry within one second; cached authority
			// still expires independently, including while a request is pending.
			if err != nil && delay > time.Second {
				delay = time.Second
			}
			timer.Reset(a.refreshDelay(delay))
		}
	}
}

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
