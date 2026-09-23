//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

// RecordTerminalJoin uses the existing signed observation transport, without
// consuming heartbeat/ENV observations or waiting for a background report tick.
func (s *runtimeObservationSender) RecordTerminalJoin(ctx context.Context, join server.TerminalJoin) error {
	body, err := json.Marshal(struct {
		EnvironmentID   string              `json:"environment_id"`
		ResourceID      string              `json:"resource_id"`
		ReporterVersion string              `json:"reporter_version"`
		SampledAt       time.Time           `json:"sampled_at"`
		TerminalJoin    server.TerminalJoin `json:"terminal_join"`
	}{s.environmentID, s.machineID, s.reporterVersion, time.Now().UTC(), join})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx)
	if err != nil {
		return err
	}
	operation, err := s.operationID()
	if err != nil {
		return err
	}
	proof, err := s.proofs.Proof(ctx, operation, http.MethodPost, "/v1/runtime-observations", body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 || response.StatusCode != http.StatusAccepted {
		return errors.New("terminal join recording unavailable")
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Recorded bool `json:"recorded"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Data.Recorded {
		return errors.New("terminal join recording unavailable")
	}
	return nil
}
