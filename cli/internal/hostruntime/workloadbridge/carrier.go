package workloadbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type CarrierRequest struct {
	PeerID    string `json:"peer_id,omitempty"`
	Exchange  []byte `json:"exchange,omitempty"`
	Operation string `json:"operation"`
	Slot      int    `json:"slot,omitempty"`
	Offer     []byte `json:"offer,omitempty"`
}
type CarrierResponse struct {
	Slot  int    `json:"slot,omitempty"`
	Offer []byte `json:"offer,omitempty"`
}

func (s *Server) carrier(ctx context.Context, body []byte) (json.RawMessage, error) {
	if !s.SessionOnly || s.CarrierControl == nil || len(body) > 16<<10 {
		return nil, session.ErrInvalidSession
	}
	var request CarrierRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return nil, session.ErrInvalidSession
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, session.ErrInvalidSession
	}
	result, err := s.CarrierControl(ctx, request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func (c *Client) TerminalCarrier(ctx context.Context, request CarrierRequest) (CarrierResponse, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return CarrierResponse{}, err
	}
	result, err := c.Call(ctx, "owner", "carrier", body)
	if err != nil {
		return CarrierResponse{}, err
	}
	var response CarrierResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return CarrierResponse{}, ownerTransportFailure{cause: err}
	}
	if response.Slot < 0 || response.Slot > 2 || len(response.Offer) > 8<<10 {
		return CarrierResponse{}, session.ErrOwnerUnavailable
	}
	return response, nil
}
