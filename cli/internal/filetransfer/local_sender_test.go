package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestLocalSenderNativeOmitsKeysAndCancelsKnownBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	known := make(chan struct{})
	var knownOnce sync.Once
	canceled := make(chan struct{}, 1)
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
	digest := sha256.Sum256([]byte("data"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/local-file-transfers":
			var raw map[string]json.RawMessage
			if json.NewDecoder(request.Body).Decode(&raw) != nil || raw["key_material"] != nil || raw["transfer_generation"] != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSONTest(writer, http.StatusCreated, map[string]any{"batch_id": "native_batch", "transfers": []Manifest{{TransferID: "ft_generated", BatchID: "native_batch", SourceMachineID: "machine_host", DestinationMachineID: "machine_cli", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "data.bin", Size: 4, SHA256: hex.EncodeToString(digest[:]), State: "created", ExpiresAt: expires}}})
		case request.Method == http.MethodGet:
			knownOnce.Do(func() { close(known) })
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", BatchID: "native_batch", Basename: "data.bin", Size: 4, State: "uploading", ExpiresAt: expires})
		case request.Method == http.MethodPatch:
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending"})
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/local-file-transfers/ft_generated":
			select {
			case canceled <- struct{}{}:
			default:
			}
			writeJSONTest(writer, http.StatusServiceUnavailable, map[string]any{"code": "cancel_unavailable", "message": "private cleanup detail"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	sender := &LocalSender{Endpoint: server.URL + "/v1/local-file-transfers", Token: strings.Repeat("t", 43), HTTPClient: server.Client()}
	done := make(chan error, 1)
	go func() {
		_, err := sender.SendNativeBatch(ctx, "native_batch", "machine_host", "machine_cli", "user_1", "ses_1", []Source{{Basename: "data.bin", Size: 4, SHA256: digest, Reader: bytes.NewReader([]byte("data"))}}, expires)
		done <- err
	}()
	<-known
	cancel()
	err := <-done
	var cleanupErr *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &cleanupErr) || cleanupErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("send error=%v, want cancellation and cleanup status", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("native sender did not cancel known resource")
	}
}

func TestLocalSenderClassifiesDeliveryDeadlineAtReceiptBoundary(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cause          error
		pollStatus     int
		recover        bool
		recoverPending bool
		wantMessage    string
	}{
		{name: "pure deadline", wantMessage: "file delivery timed out"},
		{name: "deadline with independent io failure", cause: syscall.EIO, wantMessage: "file delivery failed"},
		{name: "poll status preserved through deadline", pollStatus: http.StatusInternalServerError, wantMessage: "file delivery failed"},
		{name: "poll status recovers", pollStatus: http.StatusInternalServerError, recover: true},
		{name: "fresh status clears prior poll failure", pollStatus: http.StatusInternalServerError, recoverPending: true, wantMessage: "file delivery timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var getCalls int
			statusStarted := make(chan struct{})
			var statusStartedOnce sync.Once
			digest := sha256.Sum256([]byte("data"))
			expires := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/v1/local-file-transfers":
					writeJSONTest(writer, http.StatusCreated, map[string]any{"batch_id": "native_batch", "transfers": []Manifest{{TransferID: "ft_generated", BatchID: "native_batch", SourceMachineID: "machine_host", DestinationMachineID: "machine_cli", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "data.bin", Size: 4, SHA256: hex.EncodeToString(digest[:]), State: "created", ExpiresAt: expires}}})
				case request.Method == http.MethodGet:
					getCalls++
					if getCalls == 1 {
						writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", CommittedOffset: 4, State: "uploading", ExpiresAt: expires})
						return
					}
					statusStartedOnce.Do(func() { close(statusStarted) })
					if getCalls == 2 && tc.pollStatus != 0 {
						writeJSONTest(writer, tc.pollStatus, map[string]any{"code": "status_unavailable", "message": "private poll response"})
						return
					}
					if getCalls == 3 && tc.recoverPending {
						writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending", ExpiresAt: expires})
						return
					}
					if getCalls == 3 && tc.recover {
						writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "delivered", ResultCode: "stored", ReceiptPath: "/Paperboat Inbox/data.bin", ExpiresAt: expires})
						return
					}
					<-request.Context().Done()
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
					writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending"})
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			timeout := 500 * time.Millisecond
			if tc.recoverPending {
				timeout = 750 * time.Millisecond
			} else if tc.recover {
				timeout = 2 * time.Second
			}
			deadline := time.Now().Add(timeout)
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.cause == nil {
				ctx, cancel = context.WithDeadline(context.Background(), deadline)
			} else {
				ctx, cancel = context.WithDeadlineCause(context.Background(), deadline, tc.cause)
			}
			defer cancel()
			sender := &LocalSender{Endpoint: server.URL + "/v1/local-file-transfers", Token: strings.Repeat("t", 43), HTTPClient: server.Client()}
			type sendResult struct {
				batch Batch
				err   error
			}
			done := make(chan sendResult, 1)
			go func() {
				batch, err := sender.SendNativeBatch(ctx, "native_batch", "machine_host", "machine_cli", "user_1", "ses_1", []Source{{Basename: "data.bin", Size: 4, SHA256: digest, Reader: bytes.NewReader([]byte("data"))}}, expires)
				done <- sendResult{batch: batch, err: err}
			}()

			select {
			case <-statusStarted:
			case <-time.After(time.Second):
				t.Fatal("delivery receipt polling did not start")
			}
			result := <-done
			if tc.recover {
				if result.err != nil || result.batch.BatchID != "native_batch" || len(result.batch.Paths) != 1 || result.batch.Paths[0] != "/Paperboat Inbox/data.bin" {
					t.Fatalf("delivery did not recover after transient receipt status: batch=%+v err=%v", result.batch, result.err)
				}
				return
			}
			err := result.err
			if tc.recoverPending {
				var pollErr *Error
				if !errors.Is(err, context.DeadlineExceeded) || err.Error() != tc.wantMessage || errors.As(err, &pollErr) && pollErr.StatusCode == tc.pollStatus {
					t.Fatalf("successful status did not clear stale receipt error: %T %v", err, err)
				}
				return
			}
			if !errors.Is(err, context.DeadlineExceeded) || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("send error=%v, want deadline and independent cause %v", err, tc.cause)
			}
			var staged interface{ DiagnosticStage() string }
			var coded interface{ DiagnosticCode() string }
			if !errors.As(err, &staged) || staged.DiagnosticStage() != "delivery" || !errors.As(err, &coded) || coded.DiagnosticCode() != "file_transfer_failed" {
				t.Fatalf("delivery failure classification=%T %v", err, err)
			}
			if err.Error() != tc.wantMessage {
				t.Fatalf("delivery failure prose=%q, want %q", err, tc.wantMessage)
			}
			if tc.pollStatus != 0 {
				var pollErr *Error
				if !errors.As(err, &pollErr) || pollErr.StatusCode != tc.pollStatus {
					t.Fatalf("final delivery error lost receipt status cause: %T %v", err, err)
				}
				if strings.Contains(err.Error(), "private poll response") {
					t.Fatal("receipt error exposed remote response text")
				}
			}
		})
	}
}

func TestLocalSenderRetainsIndependentCauseJoinedWithPollDeadline(t *testing.T) {
	digest := sha256.Sum256([]byte("data"))
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
	var statusCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/local-file-transfers":
			writeJSONTest(writer, http.StatusCreated, map[string]any{"batch_id": "native_batch", "transfers": []Manifest{{TransferID: "ft_generated", BatchID: "native_batch", SourceMachineID: "machine_host", DestinationMachineID: "machine_cli", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "data.bin", Size: 4, SHA256: hex.EncodeToString(digest[:]), State: "created", ExpiresAt: expires}}})
		case request.Method == http.MethodGet:
			statusCalls++
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", CommittedOffset: 4, State: "uploading", ExpiresAt: expires})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	independentCause := errors.New("independent poll read failure")
	var requestCalls int
	httpClient := server.Client()
	baseTransport := httpClient.Transport
	httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			requestCalls++
			if requestCalls > 1 {
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        make(http.Header),
					Body:          &observedResponseBody{readErr: errors.Join(context.DeadlineExceeded, independentCause)},
					ContentLength: -1,
					Request:       request,
				}, nil
			}
		}
		return baseTransport.RoundTrip(request)
	})
	sender := &LocalSender{Endpoint: server.URL + "/v1/local-file-transfers", Token: strings.Repeat("t", 43), HTTPClient: httpClient}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := sender.SendNativeBatch(ctx, "native_batch", "machine_host", "machine_cli", "user_1", "ses_1", []Source{{Basename: "data.bin", Size: 4, SHA256: digest, Reader: bytes.NewReader([]byte("data"))}}, expires)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, independentCause) {
		t.Fatalf("delivery error=%v, deadline=%v independent=%v transport GETs=%d server status GETs=%d", err, errors.Is(err, context.DeadlineExceeded), errors.Is(err, independentCause), requestCalls, statusCalls)
	}
	if err.Error() != "file delivery failed" {
		t.Fatalf("mixed poll error was treated as pure timeout: %q", err.Error())
	}
	if requestCalls < 2 || statusCalls != 1 {
		t.Fatalf("poll requests=%d server status requests=%d, want upload status plus intercepted receipt polls", requestCalls, statusCalls)
	}
}

type lostPatchResponseTransport struct {
	next    http.RoundTripper
	patches int
}

func (t *lostPatchResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil || request.Method != http.MethodPatch {
		return response, err
	}
	t.patches++
	var closeErr error
	if response != nil && response.Body != nil {
		closeErr = response.Body.Close()
	}
	return nil, errors.Join(syscall.EIO, closeErr)
}

func TestLocalSenderRetainsPatchCauseAndRecoversFromCommittedStatus(t *testing.T) {
	for _, tc := range []struct {
		name            string
		commitBeforeEIO bool
	}{
		{name: "four uncertain patch failures preserve cause"},
		{name: "status confirms patch committed before EIO", commitBeforeEIO: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digest := sha256.Sum256([]byte("data"))
			expires := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
			var committed int64
			var completeCalls int
			var patchCalls int
			patchBodyValid := true
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost && request.URL.Path == "/v1/local-file-transfers":
					writeJSONTest(writer, http.StatusCreated, map[string]any{"batch_id": "native_batch", "transfers": []Manifest{{TransferID: "ft_generated", BatchID: "native_batch", SourceMachineID: "machine_host", DestinationMachineID: "machine_cli", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "data.bin", Size: 4, SHA256: hex.EncodeToString(digest[:]), State: "created", ExpiresAt: expires}}})
				case request.Method == http.MethodGet && request.URL.Path == "/v1/local-file-transfers/ft_generated":
					if completeCalls > 0 {
						writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "delivered", ResultCode: "stored", ReceiptPath: "/Paperboat Inbox/data.bin", ExpiresAt: expires})
						return
					}
					writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", CommittedOffset: committed, State: "uploading", ExpiresAt: expires})
				case request.Method == http.MethodPatch && request.URL.Path == "/v1/local-file-transfers/ft_generated/content":
					patchCalls++
					payload, readErr := io.ReadAll(request.Body)
					if readErr != nil || string(payload) != "data" {
						patchBodyValid = false
					}
					if tc.commitBeforeEIO {
						committed = int64(len(payload))
					}
					writer.WriteHeader(http.StatusNoContent)
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
					completeCalls++
					writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending"})
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			httpClient := server.Client()
			lost := &lostPatchResponseTransport{next: httpClient.Transport}
			httpClient.Transport = lost
			sender := &LocalSender{Endpoint: server.URL + "/v1/local-file-transfers", Token: strings.Repeat("t", 43), HTTPClient: httpClient}
			batch, err := sender.SendNativeBatch(context.Background(), "native_batch", "machine_host", "machine_cli", "user_1", "ses_1", []Source{{Basename: "data.bin", Size: 4, SHA256: digest, Reader: bytes.NewReader([]byte("data"))}}, expires)
			if !patchBodyValid {
				t.Fatal("patch request did not deliver the source bytes to the receiver")
			}
			if tc.commitBeforeEIO {
				if err != nil || batch.BatchID != "native_batch" || len(batch.Paths) != 1 || patchCalls != 1 || completeCalls != 1 {
					t.Fatalf("status recovery did not confirm the uncertain patch: batch=%+v err=%v patches=%d completes=%d", batch, err, patchCalls, completeCalls)
				}
				return
			}
			if err == nil || !errors.Is(err, syscall.EIO) || err.Error() != "local transfer did not commit all bytes" || patchCalls != 4 || lost.patches != 4 || completeCalls != 0 {
				t.Fatalf("exhausted patch errors lost their cause or bound: err=%v patches=%d transport=%d completes=%d", err, patchCalls, lost.patches, completeCalls)
			}
		})
	}
}

func writeJSONTest(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
