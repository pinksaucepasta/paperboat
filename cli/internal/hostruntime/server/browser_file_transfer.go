package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

const BrowserFileTransferChunkBytes = 32 << 10

type browserFileTransferRequest struct {
	Action     string              `json:"action"`
	BatchID    string              `json:"batch_id,omitempty"`
	Files      []filetransfer.File `json:"files,omitempty"`
	TransferID string              `json:"transfer_id,omitempty"`
	Offset     *int64              `json:"offset,omitempty"`
	Data       *string             `json:"data,omitempty"`
	SHA256     string              `json:"sha256,omitempty"`
}

func browserTransferSource(a Authorization) string {
	hash := sha256.Sum256([]byte(a.AccountID + "\x00" + a.MachineID + "\x00" + a.SessionID + "\x00" + strconv.FormatUint(a.TerminalGeneration, 10)))
	return "browser_" + hex.EncodeToString(hash[:])
}
func browserTransferOwned(a Authorization, t store.FileTransfer) bool {
	return t.SourceMachineID == browserTransferSource(a) && t.DestinationMachineID == a.MachineID && t.InitiatingUserID == a.AccountID && t.SessionID == a.SessionID && t.DeliveryClientID == ""
}

func (d *Dispatcher) browserTransferLive(a Authorization) error {
	if !a.BrowserTerminal || a.TerminalRole != TerminalRoleOwner || a.AccountID == "" || a.UserID != a.AccountID || a.MachineID == "" || a.SessionID == "" || a.TerminalGeneration == 0 || a.BrowserAttachmentID == "" || a.ClientID != a.BrowserAttachmentID || a.Revoked != nil && a.Revoked.Load() || !a.ExpiresAt.IsZero() && !d.config.Now().Before(a.ExpiresAt) {
		return session.ErrSessionUnknown
	}
	snapshot, err := d.config.Sessions.SnapshotAtGeneration(a.SessionID, a.TerminalGeneration)
	if err != nil {
		return err
	}
	if snapshot.State != session.Running {
		return session.ErrSessionUnknown
	}
	state, _, err := d.config.Sessions.AttachmentStatus(a.SessionID, a.BrowserAttachmentID)
	if err != nil || state != session.Attached {
		return session.ErrSessionUnknown
	}
	return nil
}

func browserTransferOutcome(ctx context.Context, stage string, value any, err error) operation.Outcome {
	if err == nil {
		return result(value)
	}
	code := reportFileTransferFailure(ctx, stage, err)
	if code == "not_found" {
		code = "not_found_or_forbidden"
	}
	return failure(code)
}

func (d *Dispatcher) browserFileTransfer(ctx context.Context, a Authorization, payload json.RawMessage) operation.Outcome {
	if d.config.FileTransfers == nil {
		return failure("capability_disabled")
	}
	if err := d.browserTransferLive(a); err != nil {
		return domainResult(nil, err)
	}
	var request browserFileTransferRequest
	if decodeStrict(payload, &request) != nil {
		return failure("invalid_request")
	}
	// Reject fields belonging to another action, including explicitly supplied zero values.
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return failure("invalid_request")
	}
	allowed := map[string]bool{"action": true}
	switch request.Action {
	case "policy":
	case "create":
		allowed["batch_id"] = true
		allowed["files"] = true
	case "get", "complete", "cancel":
		allowed["transfer_id"] = true
	case "chunk":
		allowed["transfer_id"] = true
		allowed["offset"] = true
		allowed["data"] = true
		allowed["sha256"] = true
	default:
		return failure("invalid_request")
	}
	for field := range fields {
		if !allowed[field] {
			return failure("invalid_request")
		}
	}
	operationCtx, cancel := context.WithCancel(ctx)
	if !a.ExpiresAt.IsZero() {
		cancel()
		operationCtx, cancel = context.WithDeadline(ctx, a.ExpiresAt)
	}
	defer cancel()
	if a.RevokedSignal != nil {
		select {
		case <-a.RevokedSignal:
			cancel()
		default:
		}
		go func() {
			select {
			case <-a.RevokedSignal:
				cancel()
			case <-operationCtx.Done():
			}
		}()
	}
	if err := operationCtx.Err(); err != nil {
		return domainResult(nil, err)
	}
	service := d.config.FileTransfers
	if request.Action == "policy" {
		return result(map[string]any{"policy": service.Policy()})
	}
	if request.Action == "create" {
		if len(request.BatchID) < 8 || len(request.BatchID) > 128 || len(request.Files) == 0 {
			return failure("invalid_request")
		}
		for _, r := range request.BatchID {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return failure("invalid_request")
			}
		}
		policy := service.Policy()
		if len(request.Files) > policy.MaxBatchFiles {
			return failure("batch_limit")
		}
		for _, file := range request.Files {
			if !browserTransferDigest(file.SHA256) {
				return failure("invalid_request")
			}
		}
		hash := sha256.Sum256([]byte(browserTransferSource(a) + "\x00" + request.BatchID))
		batchID := "browser_batch_" + hex.EncodeToString(hash[:])
		// Serialize only create/resume admission; data chunks use the service's existing slots.
		select {
		case d.browserTransferCreate <- struct{}{}:
			defer func() { <-d.browserTransferCreate }()
		case <-operationCtx.Done():
			return domainResult(nil, operationCtx.Err())
		}
		transfers, err := service.Batch(operationCtx, batchID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return browserTransferOutcome(operationCtx, "command", nil, err)
		}
		if len(transfers) > 0 {
			if len(transfers) != len(request.Files) {
				return failure("operation_conflict")
			}
			ordered := make([]store.FileTransfer, len(transfers))
			used := make([]bool, len(transfers))
			for i, file := range request.Files {
				found := false
				for j, t := range transfers {
					if !browserTransferOwned(a, t) {
						return failure("not_found_or_forbidden")
					}
					if !used[j] && t.Basename == file.Basename && t.Size == file.Size && t.SHA256 == file.SHA256 {
						ordered[i] = t
						used[j] = true
						found = true
						break
					}
				}
				if !found {
					return failure("operation_conflict")
				}
			}
			transfers = ordered
		} else {
			if err := d.browserTransferLive(a); err != nil {
				return domainResult(nil, err)
			}
			transfers, err = service.Create(operationCtx, filetransfer.CreateRequest{BatchID: batchID, SourceMachineID: browserTransferSource(a), DestinationMachineID: a.MachineID, InitiatingUserID: a.AccountID, SessionID: a.SessionID, Files: request.Files})
		}
		return browserTransferOutcome(operationCtx, "command", map[string]any{"transfers": transfers}, err)
	}
	if request.TransferID == "" || len(request.TransferID) > 128 {
		return failure("invalid_request")
	}
	transfer, err := service.Get(operationCtx, request.TransferID)
	if errors.Is(err, store.ErrNotFound) || err == nil && !browserTransferOwned(a, transfer) {
		return failure("not_found_or_forbidden")
	}
	if err != nil {
		return browserTransferOutcome(operationCtx, "peer_authority", nil, err)
	}
	// Complete and cancel operate on the entire batch; authorize every member first.
	if request.Action == "complete" || request.Action == "cancel" {
		batch, batchErr := service.Batch(operationCtx, transfer.BatchID)
		if batchErr != nil {
			return browserTransferOutcome(operationCtx, "delivery", nil, batchErr)
		}
		for _, member := range batch {
			if !browserTransferOwned(a, member) {
				return failure("not_found_or_forbidden")
			}
		}
	}
	if err := operationCtx.Err(); err != nil {
		return domainResult(nil, err)
	}
	switch request.Action {
	case "get":
		if transfer.State == "published" {
			transfer.ReceiptPath, err = service.ExistingPublishedPath(operationCtx, transfer.ID)
		}
	case "chunk":
		if request.Offset == nil || *request.Offset < 0 || request.Data == nil || len(*request.Data) > base64.StdEncoding.EncodedLen(BrowserFileTransferChunkBytes) || !browserTransferDigest(request.SHA256) {
			return failure("invalid_request")
		}
		data, decodeErr := base64.StdEncoding.Strict().DecodeString(*request.Data)
		if decodeErr != nil || len(data) == 0 || len(data) > BrowserFileTransferChunkBytes || base64.StdEncoding.EncodeToString(data) != *request.Data {
			return failure("invalid_request")
		}
		digest, _ := hex.DecodeString(request.SHA256)
		var sum [sha256.Size]byte
		copy(sum[:], digest)
		transfer, err = service.AppendVerified(operationCtx, transfer.ID, *request.Offset, bytes.NewReader(data), sum)
	case "complete":
		transfer, err = service.Complete(operationCtx, transfer.ID)
		if err == nil {
			if liveErr := d.browserTransferLive(a); liveErr != nil {
				return domainResult(nil, liveErr)
			}
			transfer.ReceiptPath, err = service.PublishedPath(operationCtx, transfer.ID)
		}
	case "cancel":
		return browserTransferOutcome(operationCtx, "delivery", struct{}{}, service.Cancel(operationCtx, transfer.ID))
	}
	return browserTransferOutcome(operationCtx, "delivery", map[string]any{"transfer": transfer}, err)
}
func browserTransferDigest(digest string) bool {
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
