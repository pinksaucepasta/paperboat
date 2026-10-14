package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestVaultLayerRecipientsFreezeExactSourceAcrossPages(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				q := r.URL.Query()
				_, hasState := q["state"]
				if r.URL.Path != "/v1/environment/layers" || q.Get("workspace") != "team-one" || q.Get("owner_kind") != "personal" || q.Get("owner_id") != "account-one" || q.Get("machine_id") != "device-one" || q.Get("limit") != "200" || hasState != pending || (pending && q.Get("state") != "pending") {
					t.Error("source discovery changed authorization coordinate")
				}
				offset, _ := strconv.Atoi(q.Get("offset"))
				items := []VaultLayerRecipient{}
				for i := offset; i < 205 && i < offset+200; i++ {
					items = append(items, VaultLayerRecipient{RecipientAccount: "account-one", MachineID: fmt.Sprintf("machine_%03d", i)})
				}
				var next *int
				if offset == 0 {
					n := 200
					next = &n
				}
				w.Header().Set("Cache-Control", "no-store")
				writeData(w, 200, map[string]any{"items": items, "pagination": Pagination{Limit: 200, Offset: offset, Total: 205, NextOffset: next}})
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{}, server.Client())
			if err := client.SetWorkspace("different-team"); err != nil {
				t.Fatal(err)
			}
			source := VaultLayerCoordinate{WorkspaceID: "team-one", OwnerKind: "personal", OwnerID: "account-one", MachineID: "device-one"}
			var items []VaultLayerRecipient
			var err error
			if pending {
				items, err = client.VaultLayerRecipients(context.Background(), source)
			} else {
				items, err = client.VaultAllLayerRecipients(context.Background(), source)
			}
			if err != nil || len(items) != 205 || requests != 2 || client.Workspace() != "different-team" {
				t.Fatalf("items=%d requests=%d error=%v", len(items), requests, err)
			}
		})
	}
}

func TestVaultTeamCapsuleManagementUsesAccountAndExactTeamWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantWorkspace := "personal"
		if r.URL.Path == "/v1/environment/teams/team-one" {
			wantWorkspace = "team-one"
		}
		if r.URL.Query().Get("workspace") != wantWorkspace {
			t.Error("capsule custody or Team read used unrelated active workspace")
		}
		w.Header().Set("Cache-Control", "no-store")
		var data any = map[string]any{"grants": []VaultGrantState{{TeamID: "team-one", Acknowledged: true}}}
		if r.URL.Path == "/v1/environment/teams/team-one" {
			data = VaultTeamState{TeamID: "team-one"}
		} else if r.URL.Path == "/v1/environment/users/account-one/sharing" {
			data = VaultSharingState{AccountID: "account-one"}
		}
		writeData(w, 200, data)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	if err := client.SetWorkspace("different-team"); err != nil {
		t.Fatal(err)
	}
	grants, err := client.GetVaultGrants(context.Background())
	if err != nil || len(grants) != 1 || !grants[0].Acknowledged {
		t.Fatal("current acknowledged capsule was not discoverable")
	}
	if err := client.AckVaultGrant(context.Background(), "digest", "vault"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetVaultSharing(context.Background(), "account-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetVaultTeam(context.Background(), "team-one"); err != nil {
		t.Fatal(err)
	}
	if client.Workspace() != "different-team" {
		t.Fatal("management changed active workspace")
	}
}
