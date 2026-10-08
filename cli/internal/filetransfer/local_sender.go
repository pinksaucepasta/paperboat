package filetransfer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type LocalSender struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
}

func (s *LocalSender) SendNativeBatch(ctx context.Context, batchID, sourceMachineID, destinationMachineID, initiatingUserID, sessionID string, sources []Source, expiresAt time.Time) (Batch, error) {
	if s == nil || ctx == nil || s.Endpoint == "" || len(s.Token) < 32 || batchID == "" || sourceMachineID == "" || destinationMachineID == "" || sourceMachineID == destinationMachineID || initiatingUserID == "" || sessionID == "" || len(sources) == 0 || len(sources) > 10 || !expiresAt.After(time.Now().UTC()) || expiresAt.Nanosecond() != 0 {
		return Batch{}, errors.New("invalid local native file transfer")
	}
	return s.sendBatch(ctx, batchID, sourceMachineID, destinationMachineID, initiatingUserID, sessionID, sources, expiresAt)
}

func (s *LocalSender) sendBatch(ctx context.Context, batchID, sourceMachineID, destinationMachineID, initiatingUserID, sessionID string, sources []Source, expiresAt time.Time) (result Batch, resultErr error) {
	files := make([]map[string]any, len(sources))
	for index, source := range sources {
		if source.Reader == nil || source.Basename == "" || source.Size < 0 {
			return Batch{}, errors.New("invalid local file source")
		}
		files[index] = map[string]any{"basename": source.Basename, "size": source.Size, "sha256": hex.EncodeToString(source.SHA256[:])}
	}
	request := map[string]any{"batch_id": batchID, "destination_machine_id": destinationMachineID, "initiating_user_id": initiatingUserID, "session_id": sessionID, "expires_at": expiresAt, "files": files}
	payload, err := json.Marshal(request)
	if err != nil {
		return Batch{}, err
	}
	client := NewClient(s.Endpoint, Auth{Token: s.Token}, Binding{SourceMachineID: sourceMachineID, DestinationMachineID: destinationMachineID, InitiatingUserID: initiatingUserID}, s.HTTPClient)
	var created struct {
		BatchID   string     `json:"batch_id"`
		Transfers []Manifest `json:"transfers"`
	}
	if err := client.retryJSONRequest(ctx, http.MethodPost, client.Endpoint, operationID("local-create", batchID), "application/json", 0, payload, &created); err != nil {
		return Batch{}, err
	}
	if created.BatchID != batchID || len(created.Transfers) != len(sources) {
		return Batch{}, errors.New("local runtime returned invalid transfer resources")
	}
	completedSuccessfully := false
	defer func() {
		if completedSuccessfully || !errors.Is(resultErr, context.Canceled) {
			return
		}
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, transfer := range created.Transfers {
			_ = client.Cancel(cancelCtx, transfer.TransferID)
		}
	}()
	seenIDs := make(map[string]struct{}, len(created.Transfers))
	for index, transfer := range created.Transfers {
		expectedDigest := hex.EncodeToString(sources[index].SHA256[:])
		_, duplicate := seenIDs[transfer.TransferID]
		if transfer.TransferID == "" || duplicate || transfer.BatchID != batchID || transfer.SourceMachineID != sourceMachineID || transfer.DestinationMachineID != destinationMachineID || transfer.InitiatingUserID != initiatingUserID || transfer.SessionID != sessionID || transfer.Basename != sources[index].Basename || transfer.Size != sources[index].Size || transfer.SHA256 != expectedDigest {
			return Batch{}, errors.New("local runtime returned invalid resource identity")
		}
		seenIDs[transfer.TransferID] = struct{}{}
		if err := s.upload(ctx, client, transfer, sources[index]); err != nil {
			return Batch{}, err
		}
	}
	for _, transfer := range created.Transfers {
		var completed Manifest
		if err := client.retryJSONRequest(ctx, http.MethodPost, client.Endpoint+"/"+transfer.TransferID+"/complete", operationID("local-complete", transfer.TransferID), "", 0, nil, &completed); err != nil {
			return Batch{}, err
		}
		if completed.State != "pending" && completed.State != "delivered" {
			return Batch{}, errors.New("local runtime did not queue file transfer")
		}
	}
	results := make([]Manifest, len(created.Transfers))
	deadline := time.NewTimer(time.Until(expiresAt))
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		remaining := 0
		for index, transfer := range created.Transfers {
			if results[index].State == "delivered" || results[index].State == "failed" {
				continue
			}
			remaining++
			current, err := client.Status(ctx, transfer.TransferID)
			if err != nil {
				continue
			}
			results[index] = current
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return Batch{}, deliveryContextError(ctx)
		case <-deadline.C:
			return Batch{}, fileTransferPhaseFailure("delivery", context.DeadlineExceeded)
		case <-ticker.C:
		}
	}
	paths := make([]string, len(results))
	for index, result := range results {
		if result.State != "delivered" || result.ResultCode != "stored" || result.ReceiptPath == "" {
			return Batch{BatchID: batchID, Transfers: results}, fileTransferPhaseFailure("delivery", errors.New("file delivery failed"))
		}
		paths[index] = result.ReceiptPath
	}
	completedSuccessfully = true
	return Batch{BatchID: batchID, Transfers: results, Paths: paths}, nil
}

func (s *LocalSender) upload(ctx context.Context, client *Client, manifest Manifest, source Source) error {
	for attempt := 0; attempt < 4; attempt++ {
		current, err := client.Status(ctx, manifest.TransferID)
		if err != nil {
			if waitErr := waitOperationRetry(ctx, attempt); waitErr != nil {
				return err
			}
			continue
		}
		if current.CommittedOffset == source.Size {
			return nil
		}
		if current.CommittedOffset < 0 || current.CommittedOffset > source.Size {
			return errors.New("local runtime returned invalid committed offset")
		}
		if err := client.patchRequest(ctx, manifest.TransferID, current.CommittedOffset, source); err != nil {
			var responseErr *Error
			if errors.As(err, &responseErr) {
				return err
			}
			continue
		}
	}
	return errors.New("local transfer did not commit all bytes")
}
