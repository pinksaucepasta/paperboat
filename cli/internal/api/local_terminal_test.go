package api

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionCreateCarriesLocalDirectoryAndOmitsRemoteDefault(t *testing.T) {
	for _, cwd := range []string{"/my project/日本", ""} {
		t.Run(cwd, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != "POST" || r.URL.Path != "/v1/machines/machine_local/connection-descriptor" {
					t.Errorf("wrong target: %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Source string            `json:"source_machine_id"`
					Create map[string]string `json:"create_session"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Source != "machine_local" || body.Create["name"] != "fresh" || body.Create["idempotency_key"] != "create-key" || body.Create["cwd"] != cwd {
					t.Errorf("unexpected request: %+v", body)
				}
				if _, ok := body.Create["cwd"]; cwd == "" && ok {
					t.Error("remote default must omit cwd")
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{AccessToken: "test-only"}, server.Client())
			client.SetSourceMachineID("machine_local")
			if _, _, err := client.UserMachineConnectionDescriptorWithSessionCreate(context.Background(), "machine_local", "fresh", "create-key", cwd); err == nil {
				t.Fatal("authorization error lost")
			}
			if !called {
				t.Fatal("request not sent")
			}
		})
	}
}
