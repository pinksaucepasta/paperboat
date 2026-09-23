package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"io"
	"net/http"
	"net/url"
	"time"
)

// validateDeviceService revalidates the exact signed actor/session/target at the
// authoritative server before probing the origin and throughout the live flow.
func (s *runtimeObservationSender) validateDeviceService(ctx context.Context, binding nativeprivate.Binding) error {
	if binding.ResourceKind != "device_service" || binding.OwnerEndpointID != s.machineID || binding.InstallationGeneration != int64(s.installationGeneration) || binding.BootID != s.lazyBootID {
		return errors.New("device service runtime identity changed")
	}
	if err := binding.Validate(time.Now().UTC()); err != nil {
		return err
	}
	s.deviceServicesMu.Lock()
	announced := binding.AnnouncementGeneration == int64(s.deviceServicesGeneration)
	s.deviceServicesMu.Unlock()
	if !announced {
		return errors.New("device service snapshot or policy changed")
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
	endpoint.Path = "/v1/runtime-device-services/authorize"
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
	if resp.StatusCode != http.StatusOK {
		return errors.New("device service authority unavailable or revoked")
	}
	var result struct {
		Data struct {
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4097))
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || !result.Data.ExpiresAt.After(time.Now().UTC()) {
		return errors.New("device service authority expired")
	}
	return nil
}
