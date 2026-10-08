package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestPendingVaultHostsExhaustsPagesInPersonalWorkspace(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		if r.URL.Path != "/v1/environment/hosts" || q.Get("workspace") != "personal" || q.Get("state") != "pending" || q.Get("limit") != "200" {
			t.Errorf("wrong host discovery query")
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		items := []VaultHostSummary{}
		for i := offset; i < 205 && i < offset+200; i++ {
			items = append(items, VaultHostSummary{MachineID: fmt.Sprintf("machine_%03d", i), State: "pending", ProjectionRevision: 1})
		}
		var next *int
		if offset == 0 {
			n := 200
			next = &n
		}
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": items, "pagination": Pagination{Limit: 200, Offset: offset, Total: 205, NextOffset: next}}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	if err := client.SetWorkspace("team-a"); err != nil {
		t.Fatal(err)
	}
	hosts, err := client.PendingVaultHosts(context.Background())
	if err != nil || len(hosts) != 205 || requests != 2 || client.workspace != "team-a" {
		t.Fatalf("host count=%d requests=%d error=%v", len(hosts), requests, err)
	}
}
func TestPendingVaultHostsRejectsRepeatedOrNonPendingRecipients(t *testing.T) {
	for _, items := range [][]VaultHostSummary{
		{{MachineID: "machine_1", State: "pending", ProjectionRevision: 1}, {MachineID: "machine_1", State: "pending", ProjectionRevision: 1}},
		{{MachineID: "machine_1", State: "ready", ProjectionRevision: 1}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			writeData(w, 200, map[string]any{"items": items, "pagination": Pagination{Limit: 200, Total: len(items)}})
		}))
		client := New(server.URL, config.Credential{}, server.Client())
		if _, err := client.PendingVaultHosts(context.Background()); err == nil {
			t.Error("invalid pending recipient accepted")
		}
		server.Close()
	}
}

func TestVaultMachineOverrideUsesPersonalAuthorizationWithoutChangingTeam(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != method || r.URL.Path != "/v1/environment/scopes/personal/account_1" || r.URL.Query().Get("workspace") != "personal" || r.URL.Query().Get("machine_id") != "foreign_machine" {
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

func TestPendingVaultHostsIncludesRegisteredEmptyInitialDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeData(w, 200, map[string]any{"items": []VaultHostSummary{{MachineID: "registered_machine", State: "pending", ProjectionRevision: 0}}, "pagination": Pagination{Limit: 200, Total: 1}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	hosts, err := client.PendingVaultHosts(context.Background())
	if err != nil || len(hosts) != 1 || hosts[0].ProjectionRevision != 0 {
		t.Fatal("registered pending initial delivery omitted")
	}
}
