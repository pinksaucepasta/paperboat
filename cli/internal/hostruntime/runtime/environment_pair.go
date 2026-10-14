//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
)

// PreparePairEnvironment provisions the installation's normal ENV recipient
// custody and verifies Personal delivery before pairing reports readiness. It
// uses the enrolled machine's proof; account passwords never enter this path.
func PreparePairEnvironment(ctx context.Context, stateRoot, controlURL string, transport http.RoundTripper) error {
	if ctx == nil || transport == nil {
		return ErrProductionInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil {
		return err
	}
	base, err := url.Parse(controlURL)
	if err != nil || registration.ServerURL != controlURL || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.Join(ErrProductionInvalid, err)
	}
	tokens, err := enrollment.NewRenewingTokenSource(enrollment.RenewingTokenConfig{ControlURL: controlURL, StateRoot: stateRoot, Transport: transport, Timeout: 15 * time.Second, OperationID: func() (string, error) {
		id, err := uuid.NewRandom()
		if err != nil {
			return "", err
		}
		return "operation_" + id.String(), nil
	}})
	if err != nil {
		return err
	}
	observations, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: controlURL, StateRoot: stateRoot, Transport: transport, Timeout: 15 * time.Second})
	if err != nil {
		return err
	}
	credentials := renewingMachineIdentity{tokens: tokens, proofs: enrollment.ProofSource{StateRoot: stateRoot}}
	service := newLayerEnvironmentService(stateRoot, base, transport, registration, credentials, observations)
	if err := service.registerLayerRecipient(ctx); err != nil {
		return err
	}
	_, err = service.layerEnvironmentForLaunch(envinject.WithLaunchContext(ctx, "personal", service.layerRecipient.RecipientAccount))
	return err
}
