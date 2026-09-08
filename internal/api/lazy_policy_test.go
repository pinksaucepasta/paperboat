package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func validLazyPolicyTestValue() LazyPolicy {
	return LazyPolicy{ID: "lap_1", Hostname: "p3000-env.example.test", AccountID: "account_1", MachineID: "machine_1", InstallationGeneration: 2, Generation: 3, Target: LazyPolicyTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "private", OwnershipMode: "persistent_port", ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func TestLazyPolicyAPIUsesExactGeneration(t *testing.T) {
	policy := validLazyPolicyTestValue()
	client := tunnelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeTunnelTestData(t, w, policy)
		case http.MethodPut:
			var input LazyPolicyUpsertRequest
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.ID != policy.ID || input.ExpectedGeneration != policy.Generation || input.OwnershipMode != "persistent_port" {
				t.Fatal("invalid upsert request")
			}
			policy.Generation++
			writeTunnelTestData(t, w, policy)
		case http.MethodDelete:
			var input struct {
				ExpectedGeneration int64 `json:"expected_generation"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.ExpectedGeneration != policy.Generation {
				t.Fatal("delete lost generation")
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	current, err := client.GetLazyPolicy(context.Background(), policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.UpsertLazyPolicy(context.Background(), LazyPolicyUpsertRequest{ID: current.ID, MachineID: current.MachineID, ExpectedGeneration: current.Generation, Target: current.Target, AccessMode: "private", OwnershipMode: "persistent_port", ExpiresAt: time.Now().UTC().Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteLazyPolicy(context.Background(), updated.ID, updated.Generation); err != nil {
		t.Fatal(err)
	}
}

func TestLazyPolicyAPIRejectsNonLoopbackBeforeRequest(t *testing.T) {
	client := tunnelTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unsafe policy reached server") })
	_, err := client.UpsertLazyPolicy(context.Background(), LazyPolicyUpsertRequest{MachineID: "machine_1", Target: LazyPolicyTarget{Scheme: "http", Address: "10.0.0.1:3000"}, AccessMode: "private", OwnershipMode: "persistent_port", ExpiresAt: time.Now().Add(time.Hour)})
	if err != ErrUnsafeTunnelResponse {
		t.Fatalf("error=%v", err)
	}
}
