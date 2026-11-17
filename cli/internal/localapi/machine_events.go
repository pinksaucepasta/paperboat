package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
)

// MachineEventPush carries only invalidation hints, never cached account data.
// The existing local socket authenticates the worker's OS account.
type MachineEventPush struct {
	MachineID string                `json:"machine_id"`
	Topics    []machineevents.Topic `json:"topics"`
	Healthy   bool                  `json:"healthy"`
}

func (p MachineEventPush) valid() bool {
	if p.MachineID == "" || len(p.MachineID) > 128 || len(p.Topics) > len(machineevents.DaemonTopics) {
		return false
	}
	for _, topic := range p.Topics {
		found := false
		for _, allowed := range machineevents.DaemonTopics {
			if topic == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Server) machineEvents(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "use POST for machine event pushes")
		return
	}
	var push MachineEventPush
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&push) != nil || decoder.Decode(&extra) != io.EOF || !push.valid() {
		writeError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid machine event push")
		return
	}
	if s.config.MachineEvents == nil || s.config.MachineEvents(r.Context(), push) != nil {
		writeError(w, http.StatusServiceUnavailable, requestID, "unavailable", "local machine event receiver is unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Client) PushMachineEvents(ctx context.Context, push MachineEventPush) error {
	if !push.valid() {
		return ErrInvalidConfig
	}
	defer c.http.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := json.Marshal(push)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://paperboat.local/v1/machine-events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return ErrTransportUnavailable
	}
	return nil
}
