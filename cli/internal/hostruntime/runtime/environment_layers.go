//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

func (s *layerEnvironmentService) layerEnvironmentForLaunch(ctx context.Context) ([]string, error) {
	binding, ok := envinject.LaunchContextFrom(ctx)
	if !ok || s == nil || s.isClosed() {
		return nil, envinject.ErrNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.registerLayerRecipient(ctx); err != nil {
		return nil, err
	}
	s.layerMu.Lock()
	defer s.layerMu.Unlock()
	if s.isClosed() {
		return nil, envinject.ErrNotReady
	}
	keys, err := productionEnvironmentKeySourceForState(s.stateRoot, s.registration)
	if err != nil {
		return nil, err
	}
	marker, ok := keys.(environmentkey.LayerGenesisMarker)
	if !ok {
		return nil, environmentkey.ErrUnavailable
	}
	path := "/v1/environment/hosts/" + url.PathEscape(s.registration.MachineID) + "/layers"
	operation, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	operationID := "operation_" + operation.String()
	body, err := json.Marshal(struct {
		OperationID    string `json:"operation_id"`
		WorkspaceID    string `json:"workspace_id"`
		ActorAccountID string `json:"actor_account_id"`
	}{operationID, binding.WorkspaceID, binding.ActorAccountID})
	if err != nil {
		return nil, err
	}
	token, err := s.credentials.Token(ctx)
	if err != nil {
		return nil, err
	}
	proof, err := s.credentials.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.ResolveReference(&url.URL{Path: path}).String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	client := &http.Client{Transport: s.transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProductionInvalid }}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, envinject.ErrNotReady
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, envinject.ErrInvalidSnapshot
	}
	var envelope struct {
		Data *api.VaultLayerContext `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Data == nil {
		return nil, envinject.ErrInvalidSnapshot
	}
	bundle := *envelope.Data
	if s.layers == nil {
		s.layers, err = envinject.NewLayerStore(envinject.LayerConfig{Path: filepath.Join(s.stateRoot, "environment", "layer-high-water.json"), Issuer: strings.TrimRight(s.base.String(), "/"), AccountID: s.layerRecipient.RecipientAccount, MachineID: s.registration.MachineID, InstallationGeneration: uint64(s.registration.InstallationGeneration), Keys: keys, Marker: marker})
		if err != nil {
			return nil, err
		}
	}
	values, err := s.layers.Environment(ctx, binding, bundle)
	_ = s.flushLayerObservationsLocked(ctx)
	return values, err
}

func (s *layerEnvironmentService) FlushLayerObservations(ctx context.Context) error {
	if ctx == nil || s == nil || s.isClosed() {
		return envinject.ErrNotReady
	}
	s.layerMu.Lock()
	defer s.layerMu.Unlock()
	return s.flushLayerObservationsLocked(ctx)
}
func (s *layerEnvironmentService) flushLayerObservationsLocked(ctx context.Context) error {
	if s.layers == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reports, err := s.layers.PendingObservations(ctx)
	if err != nil {
		return err
	}
	if len(reports) == 0 {
		return nil
	}
	workspaces := map[string][]api.VaultLayerObservation{}
	for _, report := range reports {
		workspaces[report.WorkspaceID] = append(workspaces[report.WorkspaceID], report)
	}
	names := make([]string, 0, len(workspaces))
	for workspace := range workspaces {
		names = append(names, workspace)
	}
	sort.Strings(names)
	var failures []error
	for _, workspace := range names {
		pending := workspaces[workspace]
		for len(pending) > 0 {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			n := min(len(pending), 128)
			if err := s.sendLayerObservationBatch(ctx, pending[:n]); err != nil {
				failures = append(failures, err)
			}
			pending = pending[n:]
		}
	}
	return errors.Join(failures...)
}

func (s *layerEnvironmentService) sendLayerObservationBatch(ctx context.Context, reports []api.VaultLayerObservation) error {
	if s.observationCredentials == nil {
		return ErrProductionInvalid
	}
	if len(reports) == 0 || len(reports) > 128 {
		return envinject.ErrInvalidSnapshot
	}
	reportBytes, err := json.Marshal(reports)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(reportBytes)
	operationID := "operation_layer_" + hex.EncodeToString(digest[:])
	body, err := json.Marshal(struct {
		OperationID  string                      `json:"operation_id"`
		Observations []api.VaultLayerObservation `json:"observations"`
	}{operationID, reports})
	if err != nil {
		return err
	}
	path := "/v1/environment/hosts/" + url.PathEscape(s.registration.MachineID) + "/layer-observations"
	token, err := s.observationCredentials.Token(ctx)
	if err != nil {
		return err
	}
	proof, err := s.observationCredentials.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.ResolveReference(&url.URL{Path: path}).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	client := &http.Client{Transport: s.transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProductionInvalid }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &api.APIError{Status: response.StatusCode, Code: "environment_observation_failed", Message: "The ENV observation report was not accepted. The durable report remains pending for retry."}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return envinject.ErrInvalidSnapshot
	}
	var ack struct {
		Data struct {
			OperationID string `json:"operation_id"`
			Accepted    bool   `json:"accepted"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.Data.OperationID != operationID || !ack.Data.Accepted {
		return envinject.ErrInvalidSnapshot
	}
	return s.layers.AcknowledgeObservations(ctx, reports)
}

// registerLayerRecipient enrolls only the installation recipient key. It neither
// requires a Personal vault nor downloads an account-wide plaintext projection.
func (s *layerEnvironmentService) registerLayerRecipient(ctx context.Context) error {
	s.refresh.Lock()
	defer s.refresh.Unlock()
	s.mu.RLock()
	registered := s.layerRegistered
	s.mu.RUnlock()
	if registered {
		return nil
	}
	if s.isClosed() {
		return envinject.ErrNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	keys, err := productionEnvironmentKeySourceForState(s.stateRoot, s.registration)
	if err != nil {
		return err
	}
	material, err := keys.Load(ctx)
	if err != nil {
		return err
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		return err
	}
	operation, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	operationID := "operation_" + operation.String()
	body, err := json.Marshal(struct {
		OperationID            string `json:"operation_id"`
		InstallationGeneration uint64 `json:"installation_generation"`
		HostKeyGeneration      uint64 `json:"host_key_generation"`
		HostPublic             string `json:"host_public"`
	}{operationID, uint64(s.registration.InstallationGeneration), material.Generation, base64.RawURLEncoding.EncodeToString(public[:])})
	if err != nil {
		return err
	}
	path := "/v1/environment/hosts/" + url.PathEscape(s.registration.MachineID) + "/key"
	token, err := s.credentials.Token(ctx)
	if err != nil {
		return err
	}
	proof, err := s.credentials.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base.ResolveReference(&url.URL{Path: path}).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	client := &http.Client{Transport: s.transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrProductionInvalid }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return envinject.ErrNotReady
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return envinject.ErrInvalidSnapshot
	}
	var envelope struct {
		Data *api.VaultLayerRecipient `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Data == nil {
		return envinject.ErrInvalidSnapshot
	}
	recipient := *envelope.Data
	if recipient.RecipientAccount == "" || s.registration.AccountID != "" && recipient.RecipientAccount != s.registration.AccountID || recipient.MachineID != s.registration.MachineID || recipient.InstallationGeneration != uint64(s.registration.InstallationGeneration) || recipient.HostKeyGeneration != material.Generation || recipient.HostPublic != base64.RawURLEncoding.EncodeToString(public[:]) || recipient.DeliveryGeneration != 0 || recipient.DocumentID != "" || recipient.FenceGeneration != 0 {
		return envinject.ErrInvalidSnapshot
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return envinject.ErrNotReady
	}
	s.layerRecipient = recipient
	s.layerRegistered = true
	return nil
}
