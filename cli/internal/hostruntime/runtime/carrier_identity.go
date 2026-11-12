//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/controlbackoff"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
)

type carrierIdentity struct {
	config          runtimeconfig.Config
	controlURL      *url.URL
	transport       http.RoundTripper
	identity        enrollment.RuntimeIdentity
	generation      int64
	tokens          *enrollment.RenewingTokenSource
	machineIdentity renewingMachineIdentity
	events          *machineevents.Hub
	peerEnrollment  *peeridentityenrollment.Client
	operationID     func() (string, error)
}

func loadCarrierIdentity(ctx context.Context, version string, environ func(string) string) (*carrierIdentity, error) {
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
	tokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: transport, RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, OperationID: operationID})
	if err != nil {
		return nil, err
	}
	machineIdentity := renewingMachineIdentity{tokens: tokens, proofs: enrollment.ProofSource{StateRoot: config.StateRoot}}
	// The carrier serves SSH under the signed network, so a withdrawn peer must reach it at once (F5-10): an access
	// change wakes its renewal (carrierRenewalTopics). The 30 s wait gets its own identity source.
	eventTokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: transport, RenewBefore: 10 * time.Minute, Timeout: 15 * time.Second, OperationID: operationID})
	if err != nil {
		return nil, err
	}
	events, err := machineevents.New(machineevents.Config{ControlURL: controlURL.String(), Tokens: eventTokens, Proofs: enrollment.ProofSource{StateRoot: config.StateRoot}, OperationID: operationID, Transport: transport})
	if err != nil {
		return nil, err
	}
	peerEnrollment, err := peeridentityenrollment.New(peeridentityenrollment.Config{ControlURL: controlURL.String(), StateRoot: config.StateRoot, Transport: transport, Timeout: 15 * time.Second}, machineIdentity)
	if err != nil {
		return nil, err
	}
	return &carrierIdentity{config, controlURL, transport, identity, registration.InstallationGeneration, tokens, machineIdentity, events, peerEnrollment, operationID}, nil
}
