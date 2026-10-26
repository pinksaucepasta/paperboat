//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/controlbackoff"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"net/http"
)

// ProductionCarrier owns the account's single native connection carrier. It
// neither boots a feature worker nor owns terminal, exec, or transfer state.
type ProductionCarrier struct {
	peer          *productionNativePeerService
	authorization Service
	usage         *hostBandwidthService
	ssh           *managedssh.Host
}

// NewProductionCarrier loads an already enrolled machine identity. Initial
// enrollment and current SSH target publication remain installer/worker work.
func NewProductionCarrier(ctx context.Context, version string, environ func(string) string, dispatch *server.NativeDispatchClient, ssh *managedssh.Host) (_ *ProductionCarrier, resultErr error) {
	if ctx == nil || environ == nil || dispatch == nil || ssh == nil {
		return nil, ErrProductionInvalid
	}
	config, err := runtimeconfig.FromEnv(version, environ)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	controlURL, err := validatedControlURL(environ("PAPERBOAT_CONTROL_URL"))
	if err != nil {
		return nil, err
	}
	transport, err := productionTransport(environ("PAPERBOAT_CONTROL_CA_FILE"), environ)
	if err != nil {
		return nil, err
	}
	transport = &controlbackoff.Transport{Base: transport, Host: controlURL.Host}
	identity, err := enrollment.LoadRuntimeIdentityForRenewal(config.StateRoot, time.Now().UTC())
	if err != nil || identity.MachineID != environ("PAPERBOAT_MACHINE_ID") {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: config.StateRoot})
	if err != nil {
		return nil, err
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID != identity.MachineID || registration.InstallationGeneration < 1 {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	operationID := func() (string, error) {
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		return "operation_" + id.String(), nil
	}
	tokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: transport, RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, Clock: func() time.Time { return time.Now().UTC() }, OperationID: operationID})
	if err != nil {
		return nil, err
	}
	machineIdentity := renewingMachineIdentity{tokens: tokens, proofs: enrollment.ProofSource{StateRoot: config.StateRoot}}
	peerEnrollment, err := peeridentityenrollment.New(peeridentityenrollment.Config{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: transport, Timeout: 15 * time.Second}, machineIdentity)
	if err != nil {
		return nil, err
	}
	if err := allowPendingPeerEnrollment(ctx, peerEnrollment); err != nil {
		return nil, err
	}
	fetcher, err := auth.NewHTTPJWKSFetcher(controlURL.ResolveReference(&url.URL{Path: "/.well-known/jwks.json"}).String(), []string{controlURL.Hostname()}, transport)
	if err != nil {
		return nil, err
	}
	// The carrier has a separate persisted JWKS cache from the replaceable
	// worker; two processes must never concurrently replace the same cache.
	keys, err := auth.NewJWKSCache(auth.JWKSConfig{Fetcher: fetcher, Clock: productionClock{}, TTL: 5 * time.Minute, RetainMissing: auth.DefaultRetainMissing, PersistencePath: filepath.Join(config.StateRoot, "authorization", "carrier-jwks.json")})
	if err != nil {
		return nil, err
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = keys.Refresh(refreshCtx)
	cancel()
	usageControl := clientapi.New(controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	usageControl.SetMachineAuth(machineIdentity)
	usage, err := bandwidth.Open(filepath.Join(config.StateRoot, "carrier-bandwidth-usage.json"), usageControl, newBandwidthObservation(ctx))
	if err != nil {
		return nil, err
	}
	usageService := &hostBandwidthService{recorder: usage}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, usageService.Shutdown(context.WithoutCancel(ctx)))
		}
	}()
	audit, err := auditCarrierSSH(config.StateRoot, ssh)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	peer, err := newProductionNativePeerService(productionNativePeerConfig{
		controlURL: controlURL.String(), issuer: strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), controlURL.String()), "/"),
		stateRoot: config.StateRoot, machineID: identity.MachineID, generation: uint64(registration.InstallationGeneration),
		transport: transport, identity: machineIdentity, keys: keys, dispatch: dispatch, ssh: ssh, usage: usage,
	})
	if err != nil {
		return nil, err
	}
	return &ProductionCarrier{peer: peer, authorization: serviceGroup{&jwksRefreshService{cache: keys}, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second), newOfflineAuditService(audit, controlURL, identity.MachineID, config.StateRoot, transport, tokens, operationID)}, usage: usageService, ssh: ssh}, nil
}

// auditCarrierSSH queues the SSH sessions the carrier serves (F5-12) in its own backlog file, as for the JWKS cache.
func auditCarrierSSH(stateRoot string, ssh *managedssh.Host) (*offlineaudit.Queue, error) {
	audit, err := offlineaudit.Open(filepath.Join(stateRoot, "carrier-offline-audit.json"), nil)
	if err != nil {
		return nil, err
	}
	ssh.SetAudit(SSHSessionAudit(audit))
	return audit, nil
}

func (c *ProductionCarrier) Start(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrProductionInvalid
	}
	if err := c.authorization.Start(ctx); err != nil {
		return errors.Join(err, c.usage.Shutdown(context.WithoutCancel(ctx)))
	}
	if err := c.peer.Start(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, c.authorization.Shutdown(cleanupCtx), c.usage.Shutdown(cleanupCtx))
	}
	return nil
}

// PrepareSSHRetirement fences new SSH only when all accepted bridges ended.
// The native coordinator must also drain other application operations before
// replacing the carrier process.
func (c *ProductionCarrier) PrepareSSHRetirement() error {
	if c == nil {
		return ErrProductionInvalid
	}
	return c.ssh.Retire()
}

func (c *ProductionCarrier) Shutdown(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrProductionInvalid
	}
	// Close listeners and bridges before their identity refresh and usage owner.
	return errors.Join(c.peer.Shutdown(ctx), c.authorization.Shutdown(ctx), c.usage.Shutdown(ctx))
}
