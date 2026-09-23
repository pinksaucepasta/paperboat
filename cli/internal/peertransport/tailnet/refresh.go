package tailnet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

type NetworkAPI interface {
	RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
	PeerNetworkConfiguration(context.Context, string) (api.PeerNetworkConfigurationResult, error)
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
	var pending bool
	if err := a.state(func(s *config.PeerNetworkState) error { pending = len(s.PendingKey) != 0; return nil }); err != nil {
		return err
	}
	if !pending {
		return a.Refresh(ctx, client)
	}
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
	var operation [16]byte
	if _, err := rand.Read(operation[:]); err != nil {
		return err
	}
	result, err := client.PeerNetworkConfiguration(ctx, "network_"+hex.EncodeToString(operation[:]))
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
		return err
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
	for {
		select {
		case <-a.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			request, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.Refresh(request, client)
			cancel()
			if err != nil && report != nil {
				report(err)
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
			timer.Reset(delay)
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
