package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// validateMachineService revalidates the exact signed actor/session/target at the
// authoritative server before probing the origin and throughout the live flow.
func (s *runtimeObservationSender) validateMachineService(ctx context.Context, binding nativeprivate.Binding) error {
	if binding.ResourceKind != "machine_service" || binding.OwnerEndpointID != s.machineID || binding.InstallationGeneration != int64(s.installationGeneration) || binding.BootID != s.lazyBootID {
		return admission.ErrRevoked
	}
	if err := binding.ValidateShape(); err != nil {
		return errors.Join(admission.ErrRevoked, err)
	}
	// An unrelated service starting or stopping advances the announcement generation (for access to
	// this very machine, `pb access`'s own listener does), so the binding's generation only has to be
	// no newer than the current one; what must hold is that its exact target is still announced.
	announced := s.machineServiceAnnounced(binding)
	if !announced {
		// The service may have started after the last scan: a port reused right after its previous
		// listener stopped is still listed at the server while this host has withdrawn it. Look once
		// more before refusing (Lane H: access-machine read 0 bytes from a just-started origin).
		s.machineServicesObservation(ctx)
		announced = s.machineServiceAnnounced(binding)
	}
	if !announced {
		return admission.ErrRevoked
	}
	// Outer fields identify the authenticated accessor; the expected target uses
	// the existing runtime authorization schema, without the duplicated actor IDs.
	expected := binding
	expected.UserID = ""
	expected.CLIClientSessionID = ""
	expected.AccessSessionID = ""
	body, err := json.Marshal(struct {
		EnvironmentID      string                `json:"environment_id"`
		ResourceID         string                `json:"resource_id"`
		UserID             string                `json:"user_id"`
		CLIClientSessionID string                `json:"cli_client_session_id"`
		AccessSessionID    string                `json:"access_session_id"`
		Expected           nativeprivate.Binding `json:"expected"`
	}{s.environmentID, s.machineID, binding.UserID, binding.CLIClientSessionID, binding.AccessSessionID, expected})
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(s.endpoint)
	if err != nil {
		return err
	}
	endpoint.Path = "/v1/runtime-machine-services/authorize"
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	token, err := s.tokens.Token(callCtx)
	if err != nil {
		return err
	}
	operation, err := s.operationID()
	if err != nil {
		return err
	}
	proof, err := s.proofs.Proof(callCtx, operation, http.MethodPost, endpoint.Path, body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return admission.ErrRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return errorreport.HTTPStatusFailure(resp)
	}
	var result struct {
		Data struct {
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4097))
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || result.Data.ExpiresAt.IsZero() {
		return errors.New("machine service authority response invalid")
	}
	return nil
}

// machineServiceAnnounced reports whether the announced set holds the loopback target address
// ("127.0.0.1:5432" or "[::1]:5432") of a binding.
func (s *runtimeObservationSender) machineServiceAnnounced(binding nativeprivate.Binding) bool {
	s.machineServicesMu.Lock()
	defer s.machineServicesMu.Unlock()
	return binding.AnnouncementGeneration <= int64(s.machineServicesGeneration) && machineServiceAnnounced(s.machineServicesPorts, binding.TargetAddress)
}

func machineServiceAnnounced(ports []api.MachineServicePort, address string) bool {
	host, portText, err := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil {
		return false
	}
	family := "ipv4"
	if host == "::1" {
		family = "ipv6"
	}
	for _, announced := range ports {
		if announced.Port == port && announced.AddressFamily == family {
			return true
		}
	}
	return false
}
