package filetransfer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/admission"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
	lease, err := admission.Admit(ctx, expiresAt, nil)
	if err != nil {
		return Batch{}, err
	}
	defer lease.Close()
	return s.sendBatch(lease.Context(), batchID, sourceMachineID, destinationMachineID, initiatingUserID, sessionID, sources, expiresAt)
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
			resultErr = errors.Join(resultErr, client.Cancel(cancelCtx, transfer.TransferID))
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
	pollErrors := make([]error, len(created.Transfers))
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
				var denied *Error
				if errors.As(err, &denied) && (denied.StatusCode == http.StatusUnauthorized || denied.StatusCode == http.StatusForbidden) {
					return Batch{}, fileTransferPhaseFailure("delivery", err)
				}
				if substantiveDeliveryPollError(ctx, err) {
					pollErrors[index] = err
				}
				continue
			}
			pollErrors[index] = nil
			results[index] = current
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return Batch{}, deliveryErrorWithPollCauses(deliveryContextError(ctx), pollErrors)
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

func substantiveDeliveryPollError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	fault := errorreport.ProjectFault(ctx, "paperboat-cli", "transfer", "delivery", "file_transfer_failed", err)
	return fault.Code != "" && fault.Outcome != "canceled" && !(fault.Cause == "deadline_exceeded" && fault.Errno == 0 && fault.HTTPStatus == 0 && deliveryPollDeadlineOnly(err))
}

// A deadline cause is quiet only when every leaf in the bounded transport
// error tree is itself a deadline. Projected metadata alone can hide a joined
// operational cause such as an independent socket failure.
func deliveryPollDeadlineOnly(err error) (pure bool) {
	defer func() {
		if recover() != nil {
			pure = false
		}
	}()
	remaining := 32
	var visit func(error) bool
	visit = func(current error) bool {
		if current == nil || remaining == 0 {
			return false
		}
		remaining--
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > remaining {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap())
		}
		return errors.Is(current, context.DeadlineExceeded)
	}
	return visit(err)
}

func deliveryErrorWithPollCauses(base error, pollErrors []error) error {
	causes := make([]error, 0, len(pollErrors)+1)
	if base != nil {
		causes = append(causes, base)
	}
	for _, err := range pollErrors {
		if err != nil {
			causes = append(causes, err)
		}
	}
	if len(causes) == 0 {
		return nil
	}
	if len(causes) == 1 && base != nil {
		return base
	}
	return fileTransferPhaseFailure("delivery", errors.Join(causes...))
}

func (s *LocalSender) upload(ctx context.Context, client *Client, manifest Manifest, source Source) error {
	var lastFailure error
	for attempt := 0; attempt < 4; attempt++ {
		current, err := client.Status(ctx, manifest.TransferID)
		if err != nil {
			if waitErr := waitOperationRetry(ctx, attempt); waitErr != nil {
				return &localUploadFailure{cause: errors.Join(err, waitErr)}
			}
			lastFailure = err
			continue
		}
		lastFailure = nil
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
			lastFailure = err
			continue
		}
	}
	if lastFailure != nil {
		return &localUploadFailure{cause: lastFailure}
	}
	return errors.New("local transfer did not commit all bytes")
}

type localUploadFailure struct{ cause error }

func (*localUploadFailure) Error() string { return "local transfer did not commit all bytes" }

func (e *localUploadFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
