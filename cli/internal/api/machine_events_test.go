package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// The signed-in CLI waits on a machine of its account with its bearer credential; the answer is the
// moved topics (200), nothing (204), or the shared typed error.
func TestWaitMachineEvents(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody struct {
		Topics map[string]int64 `json:"topics"`
	}
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch status {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"topics":{"inventory":7}}`))
		case http.StatusNoContent:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"user_machine_not_found","message":"Machine was not found."}}`))
		}
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "user-token"}, server.Client())

	moved, err := client.WaitMachineEvents(context.Background(), "machine_1", map[string]int64{"inventory": 3})
	if err != nil || moved["inventory"] != 7 || len(moved) != 1 {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
	if gotPath != "/v1/machines/machine_1/events/wait" || gotAuth != "Bearer user-token" || gotBody.Topics["inventory"] != 3 {
		t.Fatalf("path=%q auth=%q body=%v", gotPath, gotAuth, gotBody)
	}
	status = http.StatusNoContent
	if moved, err = client.WaitMachineEvents(context.Background(), "machine_1", nil); err != nil || len(moved) != 0 {
		t.Fatalf("204: moved=%v err=%v", moved, err)
	}
	status = http.StatusNotFound
	_, err = client.WaitMachineEvents(context.Background(), "machine_2", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "user_machine_not_found" || apiErr.Status != 404 {
		t.Fatalf("404: %v", err)
	}
	if _, err = client.WaitMachineEvents(context.Background(), "", nil); err == nil {
		t.Fatal("empty machine id accepted")
	}
}
