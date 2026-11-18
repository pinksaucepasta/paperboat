package server

import (
	"bytes"
	"context"
	"encoding/json"
	clienttransfer "github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
)

func TestNativeTransferStreamsOneBodyAndVerifiesTheWholeFileAtComplete(t *testing.T) {
	for _, scenario := range []string{"valid", "corrupt", "oversize", "unauthorized"} {
		t.Run(scenario, func(t *testing.T) {
			base, durable := fileTransferTestHandler(t)
			handler, err := NewNativeFileTransferHandler(base.config)
			if err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte("a"), protocol.FileTransferChunkBytes)
			created, err := handler.config.Service.Create(t.Context(), filetransfer.CreateRequest{BatchID: "native_chunk", SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "test.bin", Size: int64(len(data)), SHA256: transferDigest(data)}}})
			if err != nil {
				t.Fatal(err)
			}
			id := created[0].ID
			body := data
			switch scenario {
			case "corrupt":
				body = bytes.Repeat([]byte("b"), len(data))
			case "oversize":
				body = append(bytes.Clone(data), 'a')
			}
			request := transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", body)
			request.Header.Set("Content-Type", "application/offset+octet-stream")
			request.Header.Set(HeaderUploadOffset, "0")
			if scenario == "unauthorized" {
				request.Header.Set("Authorization", "Bearer wrong")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			current, err := durable.FileTransfer(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "valid", "corrupt":
				// The body is one stream with no digest: what arrived is committed, and only Complete's
				// whole-file hash decides whether those bytes are the file.
				if response.Code != http.StatusNoContent || current.CommittedOffset != int64(len(data)) || response.Header().Get(HeaderUploadOffset) != strconv.Itoa(len(data)) {
					t.Fatalf("status=%d offset=%d", response.Code, current.CommittedOffset)
				}
				_, err := handler.config.Service.Complete(t.Context(), id)
				if (scenario == "valid") != (err == nil) {
					t.Fatalf("complete err=%v", err)
				}
			default:
				if response.Code < 400 || current.CommittedOffset != 0 {
					t.Fatalf("rejected body status=%d offset=%d", response.Code, current.CommittedOffset)
				}
			}
		})
	}
}

func TestNativeTransferPolicyUsesAuthorizedReceiver(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	policy := filetransfer.DefaultPolicy
	expected := clienttransfer.Policy{Revision: policy.Revision, MaxBatchFiles: policy.MaxBatchFiles, MaxConcurrentTransfers: policy.MaxConcurrentTransfers, RetentionSeconds: policy.RetentionSeconds, DeliveryTimeoutSeconds: policy.DeliveryTimeoutSeconds, MaxPendingSpoolBytes: policy.MaxPendingSpoolBytes}
	native, err := clienttransfer.NewNativeClient(endpoint.URL+"/v1/file-transfers", clienttransfer.Auth{Token: "token"}, clienttransfer.Binding{SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1"}, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", endpoint.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	if err := native.VerifyPolicy(t.Context(), expected); err != nil {
		t.Fatal(err)
	}
	expected.Revision = "different"
	if err := native.VerifyPolicy(t.Context(), expected); err == nil {
		t.Fatal("mismatched policy admitted")
	}
	for _, scenario := range []struct {
		method, token string
		status        int
	}{{http.MethodGet, "wrong", http.StatusUnauthorized}, {http.MethodPost, "token", http.StatusMethodNotAllowed}} {
		req := transferRequest(scenario.method, endpoint.URL+"/v1/file-transfers/policy", nil)
		req.Header.Set("Authorization", "Bearer "+scenario.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != scenario.status {
			t.Fatalf("status=%d want=%d", response.Code, scenario.status)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transferRequest(http.MethodGet, endpoint.URL+"/v1/file-transfers/policy", nil))
	var body struct {
		Policy filetransfer.Policy `json:"file_transfer_policy"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Policy != policy {
		t.Fatal("receiver limits differ")
	}
}

func TestNativeTransferCompletedCancellationReturnsConflictAndPreservesTheRecord(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	data := []byte("published record")
	created, err := handler.config.Service.Create(t.Context(), filetransfer.CreateRequest{BatchID: "completed_cancel", SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "completed.txt", Size: int64(len(data)), SHA256: transferDigest(data)}}})
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	if _, err := handler.config.Service.Append(t.Context(), id, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.config.Service.Complete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, transferRequest(http.MethodDelete, "http://helper.test/v1/file-transfers/"+id, nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != "state_conflict" || body.Retryable {
		t.Fatalf("error=%s", response.Body.String())
	}
	// The record stays published: the refused cancel changed nothing.
	if current, err := handler.config.Service.Get(t.Context(), id); err != nil || current.State != "published" {
		t.Fatalf("record after the refused cancel = %+v (err=%v)", current, err)
	}
}
