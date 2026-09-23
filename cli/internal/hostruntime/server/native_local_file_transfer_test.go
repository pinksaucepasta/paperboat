package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

func TestNativeLocalFileTransferIsIdempotentAndHasNoKeyDependency(t *testing.T) {
	base, _ := fileTransferTestHandler(t)
	recipient := "cli_live"
	handler, err := NewNativeLocalFileTransferHandler(LocalFileTransferConfig{
		Token: strings.Repeat("n", 43), MachineID: "machine_host", Service: base.config.Service,
		ResolveRecipient: func(sessionID, machineID string) (string, error) { return recipient, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	firstData, secondData := []byte("native-one"), []byte("native-two")
	files := []filetransfer.File{
		{Basename: "one.txt", Size: int64(len(firstData)), SHA256: transferDigest(firstData)},
		{Basename: "two.txt", Size: int64(len(secondData)), SHA256: transferDigest(secondData)},
	}
	payload, _ := json.Marshal(map[string]any{
		"batch_id": "native_batch", "destination_machine_id": "machine_client", "initiating_user_id": "user_1", "session_id": "ses_live",
		"expires_at": time.Now().UTC().Truncate(time.Second).Add(time.Minute),
		"files":      files,
	})
	request := func(method, target string, body []byte) *http.Request {
		r := httptest.NewRequest(method, "http://local.test/v1/local-file-transfers"+target, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("n", 43))
		return r
	}
	create := func(body []byte) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request(http.MethodPost, "", body))
		return response
	}
	first := create(payload)
	if first.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", first.Code, first.Body.String())
	}
	var created struct {
		Transfers []struct {
			ID       string `json:"transfer_id"`
			Basename string `json:"basename"`
		} `json:"transfers"`
	}
	if json.Unmarshal(first.Body.Bytes(), &created) != nil || len(created.Transfers) != 2 || created.Transfers[0].ID == "" || created.Transfers[1].ID == "" || created.Transfers[0].ID == created.Transfers[1].ID {
		t.Fatalf("created=%s", first.Body.String())
	}
	reorderedPayload, _ := json.Marshal(map[string]any{
		"batch_id": "native_batch", "destination_machine_id": "machine_client", "initiating_user_id": "user_1", "session_id": "ses_live",
		"expires_at": time.Now().UTC().Truncate(time.Second).Add(time.Minute), "files": []filetransfer.File{files[1], files[0]},
	})
	second := create(reorderedPayload)
	var recovered struct {
		Transfers []struct {
			Basename string `json:"basename"`
		} `json:"transfers"`
	}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &recovered) != nil || len(recovered.Transfers) != 2 || recovered.Transfers[0].Basename != "two.txt" || recovered.Transfers[1].Basename != "one.txt" {
		t.Fatalf("recover=%d %s", second.Code, second.Body.String())
	}
	transfer, err := base.config.Service.Get(context.Background(), created.Transfers[0].ID)
	if err != nil || transfer.SourceMachineID != "machine_host" || transfer.DeliveryClientID != "cli_live" || transfer.SessionID != "ses_live" || transfer.E2EETransferID != "" || transfer.TransferGeneration != 0 {
		t.Fatalf("transfer=%+v err=%v", transfer, err)
	}
	recipient = "cli_replaced"
	if response := create(payload); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "idempotency_conflict") {
		t.Fatalf("recipient drift=%d %s", response.Code, response.Body.String())
	}
	recipient = "cli_live"
	unauthorized := httptest.NewRequest(http.MethodGet, "http://local.test/v1/local-file-transfers/"+created.Transfers[0].ID, nil)
	unauthorized.Header.Set("Authorization", "Bearer invalid-token")
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusNotFound {
		t.Fatalf("invalid token status=%d", unauthorizedResponse.Code)
	}

	var keyed map[string]any
	_ = json.Unmarshal(payload, &keyed)
	keyed["key_material"] = "forbidden"
	keyedPayload, _ := json.Marshal(keyed)
	if response := create(keyedPayload); response.Code != http.StatusBadRequest {
		t.Fatalf("native key material accepted: %d %s", response.Code, response.Body.String())
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request(http.MethodDelete, "/"+created.Transfers[0].ID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel=%d %s", response.Code, response.Body.String())
	}
	transfer, _ = base.config.Service.Get(context.Background(), created.Transfers[0].ID)
	if transfer.State != "canceled" {
		t.Fatalf("state=%s", transfer.State)
	}
}
