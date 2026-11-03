//go:build darwin || linux || windows

package runtime

import (
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

func terminalCarrierAuthorization(base *carrierIdentity, name, issuer, version string) (*auth.JWKSCache, server.AuthorizerFactory, Service, server.TerminalJoinRecorder, error) {
	keys, err := productionJWKSCache(base.controlURL, base.transport, filepath.Join(base.config.StateRoot, "authorization", name+"-jwks.json"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	revocations := auth.NewRevocationCache()
	proofs := enrollment.ProofSource{StateRoot: base.config.StateRoot}
	refresh, err := newRevocationRefreshService(base.controlURL.ResolveReference(&url.URL{Path: "/v1/helper-trust/revocations"}).String(), base.tokens, proofs, base.operationID, revocations, base.transport, 15*time.Second)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	refresh.events = base.events.Subscribe(machineevents.TopicMachineAccess)
	verifier := auth.Verifier{Keys: keys, Clock: productionClock{}, Replays: auth.NewReplayCache(4096, productionClock{}), Revocations: revocations, ClockSkew: 30 * time.Second, RefreshTimeout: 2 * time.Second}
	authorizer, err := NewCredentialAuthorizer(CredentialAuthConfig{
		InstallationGeneration: base.generation, Issuer: issuer, EnvironmentID: base.identity.EnvironmentID,
		MachineID: base.identity.MachineID, HelperID: base.identity.HelperID, Verifier: verifier, Revocations: revocations,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sender := &runtimeObservationSender{
		endpoint:      base.controlURL.ResolveReference(&url.URL{Path: "/v1/runtime-observations"}).String(),
		environmentID: base.identity.EnvironmentID, machineID: base.identity.MachineID, reporterVersion: version,
		tokens: base.tokens, proofs: proofs, operationID: base.operationID,
		client: &http.Client{Transport: base.transport, Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect},
	}
	return keys, authorizer, serviceGroup{base.events, &jwksRefreshService{cache: keys}, refresh}, sender.RecordTerminalJoin, nil
}
