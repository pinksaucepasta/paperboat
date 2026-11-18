package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// SendPrepareRequest is the combined send setup for `pb send` to another machine: what CreateTeamInboxRequest
// (POST /v1/team-inbox/requests), the account's transfer-mode preference (GET /v1/me/preferences) and issuing the
// descriptor (POST /v1/machines/{id}/file-transfer-descriptor) each needed, in one body and one round trip. The
// destination is resolved by the caller (workspace selection and ambiguity are the CLI's to handle).
type SendPrepareRequest struct {
	DestinationMachineID string
	SourceMachineID      string
	SessionID            string
	RequestID            string
	OperationID          string
	BatchID              string
	Files                []TeamInboxFile
	ExpiresAt            time.Time
	// TransferMode is "unlimited" for the --unlimited flag, else "" for the account's preference: the server reads it,
	// which removes the standalone preference call from the send path.
	TransferMode  string
	DeclaredBytes int64
}

// SendPrepareResponse is POST /v1/sends/prepare's answer: the inbox request with its approval status, the effective
// transfer mode, and the file-transfer descriptor when the send may start at once. Descriptor is nil while the
// request waits for manual approval or when the files are stored for later pickup.
type SendPrepareResponse struct {
	Request          TeamInboxRequest `json:"request"`
	FileTransferMode string           `json:"file_transfer_mode"`
	Descriptor       *FileTransfer    `json:"descriptor"`
}

// PrepareSend answers 201 with an inline descriptor for an approved or no-approval request, 202 with no descriptor
// while approval is pending, and 201 with stored=true and no descriptor for a stored send. The server's stable error
// codes are kept in the returned *APIError, so callers branch exactly as for the calls this folds (machine_offline is
// waited out by retryWhileMachineOffline). Repeating a lost answer is safe: the operation ID resolves to the same request.
func (c *Client) PrepareSend(ctx context.Context, in SendPrepareRequest) (SendPrepareResponse, error) {
	var out SendPrepareResponse
	if strings.TrimSpace(in.DestinationMachineID) == "" || strings.TrimSpace(in.SourceMachineID) == "" || strings.TrimSpace(in.RequestID) == "" || strings.TrimSpace(in.OperationID) == "" || strings.TrimSpace(in.BatchID) == "" || len(in.Files) == 0 {
		return out, errors.New("send prepare requires a destination, source machine, request, operation and batch IDs, and files")
	}
	body := map[string]any{
		"destination_machine_id": in.DestinationMachineID,
		"source_machine_id":      in.SourceMachineID,
		"session_id":             in.SessionID,
		"request_id":             in.RequestID,
		"operation_id":           in.OperationID,
		"batch_id":               in.BatchID,
		"files":                  in.Files,
		"expires_at":             in.ExpiresAt,
		"transfer_mode":          in.TransferMode,
		"declared_bytes":         in.DeclaredBytes,
	}
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/sends/prepare", body, &out, http.Header{"Idempotency-Key": []string{in.OperationID}})
	return out, err
}
