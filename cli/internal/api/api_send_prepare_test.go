package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

func prepareTestServer(t *testing.T, status int, data any, check func(r *http.Request, body map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sends/prepare" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode prepare body: %v", err)
		}
		if check != nil {
			check(r, body)
		}
		writeData(w, status, data)
	}))
}

// A 201 approved prepare carries the resolved machine, the approved request,
// the effective mode and the inline descriptor, with the operation ID in both
// the body and the Idempotency-Key header.
func TestPrepareSendApprovedCarriesRequestModeAndDescriptor(t *testing.T) {
	var header, operationID string
	var expires string
	server := prepareTestServer(t, http.StatusCreated, map[string]any{
		"request":            map[string]any{"request_id": "request_1", "status": "approved", "manifest_digest": "digest_1", "stored": false, "team_id": "team_1", "destination_machine_id": "machine_dst"},
		"file_transfer_mode": "automatic",
		"descriptor":         map[string]any{"endpoint": "https://machine.example.test/v1/file-transfers", "source_machine_id": "machine_src", "destination_machine_id": "machine_dst", "initiating_user_id": "user_1", "auth": map[string]any{"method": "bearer", "token": "transfer-token", "access_session_id": "access_1", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}, "policy": map[string]any{"revision": "r1", "max_batch_files": 10}},
	}, func(r *http.Request, body map[string]any) {
		header, operationID = r.Header.Get("Idempotency-Key"), body["operation_id"].(string)
		expires, _ = body["expires_at"].(string)
		if body["destination_machine_id"] != "machine_dst" || body["source_machine_id"] != "machine_src" || body["request_id"] != "request_1" || body["batch_id"] != "batch_1" {
			t.Errorf("prepare body identity=%v", body)
		}
		if body["transfer_mode"] != "" || body["declared_bytes"] != float64(12) {
			t.Errorf("prepare body mode/bytes=%v", body)
		}
		files, _ := body["files"].([]any)
		if len(files) != 1 {
			t.Errorf("prepare body files=%v", body["files"])
		}
	})
	defer server.Close()
	out, err := New(server.URL, config.Credential{AccessToken: "token"}, nil).PrepareSend(context.Background(), SendPrepareRequest{
		DestinationMachineID: "machine_dst", SourceMachineID: "machine_src", RequestID: "request_1", OperationID: "operation_1", BatchID: "batch_1",
		Files: []TeamInboxFile{{Basename: "a.bin", Size: 12, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}, ExpiresAt: time.Now().Add(10 * time.Minute), DeclaredBytes: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if header != "operation_1" || operationID != "operation_1" {
		t.Fatalf("operation id header=%q body=%q", header, operationID)
	}
	if expires == "" {
		t.Fatal("expires_at missing from prepare body")
	}
	if out.Request.Status != "approved" || out.Request.ManifestDigest != "digest_1" || out.Request.Stored {
		t.Fatalf("request=%+v", out.Request)
	}
	if out.FileTransferMode != "automatic" {
		t.Fatalf("mode=%q", out.FileTransferMode)
	}
	if out.Descriptor == nil || out.Descriptor.Endpoint != "https://machine.example.test/v1/file-transfers" || out.Descriptor.Auth.Token != "transfer-token" {
		t.Fatalf("descriptor=%+v", out.Descriptor)
	}
}

// A 202 pending prepare has a null descriptor; the approval poll and the
// descriptor fetch stay separate calls.
func TestPrepareSendPendingHasNoDescriptor(t *testing.T) {
	server := prepareTestServer(t, http.StatusAccepted, map[string]any{
		"request":            map[string]any{"request_id": "request_2", "status": "pending"},
		"file_transfer_mode": "unlimited",
		"descriptor":         nil,
	}, nil)
	defer server.Close()
	out, err := New(server.URL, config.Credential{AccessToken: "token"}, nil).PrepareSend(context.Background(), SendPrepareRequest{
		DestinationMachineID: "machine_dst", SourceMachineID: "machine_src", TransferMode: "unlimited", RequestID: "request_2", OperationID: "operation_2", BatchID: "batch_2",
		Files: []TeamInboxFile{{Basename: "a.bin", Size: 1, SHA256: "sha"}}, ExpiresAt: time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Request.Status != "pending" || out.Descriptor != nil || out.FileTransferMode != "unlimited" {
		t.Fatalf("pending=%+v", out)
	}
}

// The server's stable refusal codes survive PrepareSend typed, so the send
// maps transfer_too_large and waits out machine_offline exactly as before.
func TestPrepareSendPreservesTypedRefusals(t *testing.T) {
	offline := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if offline {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"machine_offline","message":"offline"}}`)
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"error":{"code":"transfer_too_large","message":"too large","details":{"total_bytes":3400000000,"max_bytes":2000000000}}}`)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, nil)
	request := SendPrepareRequest{DestinationMachineID: "machine_dst", SourceMachineID: "machine_src", RequestID: "request_3", OperationID: "operation_3", BatchID: "batch_3", Files: []TeamInboxFile{{Basename: "a.bin", Size: 1, SHA256: "sha"}}, ExpiresAt: time.Now()}
	_, err := client.PrepareSend(context.Background(), request)
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal.Code != "transfer_too_large" {
		t.Fatalf("too-large err=%v", err)
	}
	offline = true
	_, err = client.PrepareSend(context.Background(), request)
	if !errors.As(err, &refusal) || refusal.Code != "machine_offline" {
		t.Fatalf("offline err=%v", err)
	}
}

// An invalid prepare is refused before any HTTP call.
func TestPrepareSendValidatesWithoutCallingTheServer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer server.Close()
	_, err := New(server.URL, config.Credential{AccessToken: "token"}, nil).PrepareSend(context.Background(), SendPrepareRequest{})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
