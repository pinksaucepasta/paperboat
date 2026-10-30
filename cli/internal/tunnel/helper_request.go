package tunnel

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"strings"
	"time"
)

func (c *helperTerminalConn) requestSync(ctx context.Context, capability string, payload json.RawMessage) (helperFrame, error) {
	operationCtx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
	defer cancel()
	requestID := helperID("request")
	frame := helperFrame{Type: "request", RequestID: requestID, Version: helperProtocolVersion, OperationID: helperID("operation"), Capability: capability, DeadlineMS: uint32(min(helperRequestTimeout, deadlineRemaining(operationCtx)) / time.Millisecond), Payload: payload}
	if frame.DeadlineMS == 0 {
		frame.DeadlineMS = 1
	}
	writeEnd := diagnostics.ReconnectStep(ctx, "attach_request")
	writeErr := writeHelperFrame(operationCtx, c.message, frame)
	writeEnd(writeErr)
	if err := writeErr; err != nil {
		return helperFrame{}, err
	}
	ackEnd := diagnostics.ReconnectStep(ctx, "attach_ack")
	response, err := awaitHelperResponse(operationCtx, c.message, requestID, &c.initialOutput)
	ackEnd(err)
	if err != nil {
		return helperFrame{}, err
	}
	if response.Type == "error" {
		return helperFrame{}, decodeHelperError(response)
	}
	return response, nil
}

func helperResponseSessionExisting(frame helperFrame) bool {
	var response struct {
		Result struct {
			Existing bool `json:"existing"`
		} `json:"result"`
	}
	_ = json.Unmarshal(frame.Payload, &response)
	return response.Result.Existing
}

func helperResponseSessionState(frame helperFrame) (string, uint64, uint64) {
	var response struct {
		Result struct {
			State          string `json:"state"`
			Generation     uint64 `json:"generation"`
			LatestSequence uint64 `json:"latest_sequence"`
		} `json:"result"`
	}
	_ = json.Unmarshal(frame.Payload, &response)
	return response.Result.State, response.Result.Generation, response.Result.LatestSequence
}

func helperResponseGeneration(frame helperFrame) uint64 {
	var response struct {
		Result struct {
			Generation uint64 `json:"generation"`
		} `json:"result"`
	}
	_ = json.Unmarshal(frame.Payload, &response)
	return response.Result.Generation
}

func helperResponseRuntimeVersion(frame helperFrame) string {
	var response struct {
		Result struct {
			Version string `json:"version"`
		} `json:"result"`
	}
	if frame.Type != "response" || json.Unmarshal(frame.Payload, &response) != nil {
		return ""
	}
	return strings.TrimSpace(response.Result.Version)
}
