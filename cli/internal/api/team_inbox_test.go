package api

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestTeamInboxRequestKeepsRequestOperationBatchAndApprovalBinding(t *testing.T) {
	request := TeamInboxRequest{RequestID: "request_" + uuid.NewString(), BatchID: "batch_" + uuid.NewString(), SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", Files: []TeamInboxFile{{Basename: "data", Size: 4, SHA256: "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"}}, ExpiresAt: time.Now().UTC().Add(time.Minute)}
	operationID := "operation_" + uuid.NewString()
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Error("missing authorization")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/team-inbox/requests":
			var got struct {
				TeamInboxRequest
				OperationID string `json:"operation_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
				return
			}
			if got.OperationID != operationID || got.RequestID != request.RequestID || got.BatchID != request.BatchID || got.SourceMachineID != request.SourceMachineID || got.DestinationMachineID != request.DestinationMachineID || !got.ExpiresAt.Equal(request.ExpiresAt) || !reflect.DeepEqual(got.Files, request.Files) {
				t.Error("request, operation or manifest binding changed")
			}
			creates++
			result := request
			result.Status = "pending"
			result.ManifestDigest = "manifest_bound"
			writeData(w, http.StatusCreated, result)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/team-inbox/requests/"+request.RequestID:
			result := request
			result.Status = "accepted"
			result.ManifestDigest = "manifest_bound"
			result.DecisionGeneration = 2
			writeData(w, http.StatusOK, result)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "test-access"}, server.Client())
	for range 2 {
		result, err := client.CreateTeamInboxRequest(context.Background(), request, operationID)
		if err != nil || result.RequestID != request.RequestID || result.BatchID != request.BatchID || result.ManifestDigest != "manifest_bound" {
			t.Fatalf("create lost identity/binding: %+v %v", result, err)
		}
	}
	result, err := client.TeamInboxRequest(context.Background(), request.RequestID)
	if err != nil || creates != 2 || result.RequestID != request.RequestID || result.BatchID != request.BatchID || result.Status != "accepted" || result.DecisionGeneration != 2 || result.ManifestDigest != "manifest_bound" || !reflect.DeepEqual(result.Files, request.Files) {
		t.Fatalf("approval lost identity/binding: %+v %v", result, err)
	}
}
