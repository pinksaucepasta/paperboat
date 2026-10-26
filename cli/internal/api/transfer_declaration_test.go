package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

// PLAN 3b File transfer modes: a send declares its mode and batch total; the server signs automatic with
// max_bytes = the declared total. A request that declares nothing carries neither field.
func TestFileTransferDescriptorDeclaresModeAndTotal(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": FileTransfer{Endpoint: "https://m.example.test/v1/file-transfers", Auth: AuthMaterial{Method: "bearer", Token: "t", ExpiresAt: time.Now().Add(time.Hour)}}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if _, err := client.MachineFileTransferDescriptorForRequest(context.Background(), "machine_dst", "machine_src", "", "", "", TransferDeclaration{Mode: "automatic", Bytes: 1234}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.MachineFileTransferDescriptor(context.Background(), "machine_dst", "machine_src", ""); err != nil {
		t.Fatal(err)
	}
	if bodies[0]["transfer_mode"] != "automatic" || bodies[0]["declared_bytes"] != float64(1234) {
		t.Fatalf("declared body=%v", bodies[0])
	}
	if _, ok := bodies[1]["transfer_mode"]; ok {
		t.Fatalf("undeclared body=%v", bodies[1])
	}
}
