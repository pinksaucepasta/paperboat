package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestVaultMachineOverrideUsesPrivateActiveWorkspace(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != method || r.URL.Path != "/v1/environment/scopes/personal/account_1" || r.URL.Query().Get("workspace") != "team-one" || r.URL.Query().Get("machine_id") != "foreign_machine" {
					t.Error("override request widened source or workspace")
				}
				writeData(w, 403, map[string]any{})
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{}, server.Client())
			if err := client.SetWorkspace("team-one"); err != nil {
				t.Fatal(err)
			}
			var err error
			if method == http.MethodGet {
				_, err = client.GetVaultScope(context.Background(), "personal", "account_1", "foreign_machine")
			} else {
				_, err = client.PutVaultScope(context.Background(), "personal", "account_1", "foreign_machine", VaultScopePut{})
			}
			if err == nil || requests != 1 || client.workspace != "team-one" {
				t.Fatal("owner denial or selected workspace was lost")
			}
		})
	}
}
