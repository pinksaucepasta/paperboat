package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestNativeTransferCommitsOnlyBoundedVerifiedChunks(t *testing.T) {
	for _, scenario := range []string{"valid", "corrupt", "missing_digest", "oversize", "unauthorized"} {
		t.Run(scenario, func(t *testing.T) {
			base, durable := fileTransferTestHandler(t)
			handler, err := NewNativeFileTransferHandler(base.config)
			if err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte("a"), protocol.FileTransferChunkBytes)
			if scenario == "oversize" {
				data = append(data, 'a')
			}
			created, err := handler.config.Service.Create(t.Context(), filetransfer.CreateRequest{BatchID: "native_chunk", SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "test.bin", Size: int64(len(data)), SHA256: transferDigest(data)}}})
			if err != nil {
				t.Fatal(err)
			}
			id := created[0].ID
			request := transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", data)
			request.Header.Set("Content-Type", "application/offset+octet-stream")
			request.Header.Set(HeaderUploadOffset, "0")
			digest := sha256.Sum256(data)
			request.Header.Set(HeaderUploadDigest, "sha256="+hex.EncodeToString(digest[:]))
			switch scenario {
			case "corrupt":
				request.Header.Set(HeaderUploadDigest, "sha256="+transferDigest([]byte("different")))
			case "missing_digest":
				request.Header.Del(HeaderUploadDigest)
			case "unauthorized":
				request.Header.Set("Authorization", "Bearer wrong")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			current, err := durable.FileTransfer(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "valid" {
				if response.Code != http.StatusNoContent || current.CommittedOffset != int64(len(data)) {
					t.Fatalf("status=%d offset=%d", response.Code, current.CommittedOffset)
				}
			} else if response.Code < 400 || current.CommittedOffset != 0 {
				t.Fatalf("rejected chunk status=%d offset=%d", response.Code, current.CommittedOffset)
			}
		})
	}
}

func TestNativeTransferPolicyUsesAuthorizedReceiver(t *testing.T) {
	handler, _ := fileTransferTestHandler(t)
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	policy := filetransfer.DefaultPolicy
	expected := clienttransfer.Policy{Revision: policy.Revision, MaxFileBytes: policy.MaxFileBytes, MaxBatchFiles: policy.MaxBatchFiles, MaxBatchBytes: policy.MaxBatchBytes, MaxConcurrentTransfers: policy.MaxConcurrentTransfers, RetentionSeconds: policy.RetentionSeconds, DeliveryTimeoutSeconds: policy.DeliveryTimeoutSeconds, MaxPendingSpoolBytes: policy.MaxPendingSpoolBytes}
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

func TestNativeTransferCompletedCancellationReturnsConflictAndPreservesContent(t *testing.T) {
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
	content, _, err := handler.config.Service.OpenContent(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	got, err := io.ReadAll(content)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content lost: %v", err)
	}
}
