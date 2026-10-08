package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestMachineMaintenanceInventoryForwardsContinuationAndLiteralQuery(t *testing.T) {
	const query = "fixture_% & exact?name"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		values := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/machines/machine_fixture/maintenance-approvals" || r.Header.Get("Authorization") == "" || values.Get("workspace") != "personal" || values.Get("q") != query || values.Get("state") != "pending" {
			t.Errorf("wrong authorized inventory request: %s %s", r.Method, r.URL.String())
		}
		offset, _ := strconv.Atoi(values.Get("offset"))
		limit, _ := strconv.Atoi(values.Get("limit"))
		wantLimit := 50
		if calls == 2 {
			wantLimit = 1
		}
		if limit != wantLimit || offset != (calls-1)*50 {
			t.Errorf("pagination arguments changed: %s", r.URL.RawQuery)
		}
		approvals := []map[string]any{}
		for i := offset; i < min(offset+limit, 51); i++ {
			approvals = append(approvals, map[string]any{"schema": "paperboat.machine-maintenance-approval/v1", "id": fmt.Sprintf("approval_%d", i), "machine_id": "machine_fixture", "status": "pending"})
		}
		pagination := Pagination{Limit: limit, Offset: offset, Total: 51}
		if offset+len(approvals) < 51 {
			next := offset + len(approvals)
			pagination.NextOffset = &next
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"approvals": approvals, "pagination": pagination}})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	first, err := client.MachineMaintenanceApprovals(context.Background(), "machine_fixture", 0, 0, query, "pending")
	if err != nil {
		t.Fatal(err)
	}
	page, ok := first["pagination"].(map[string]any)
	if !ok || page["limit"] != float64(50) || page["offset"] != float64(0) || page["total"] != float64(51) || page["next_offset"] != float64(50) || len(first["approvals"].([]any)) != 50 || calls != 1 {
		t.Fatalf("first page metadata missing or consumer guessed pages: page=%v calls=%d", page, calls)
	}
	second, err := client.MachineMaintenanceApprovals(context.Background(), "machine_fixture", 1, int(page["next_offset"].(float64)), query, "pending")
	if err != nil {
		t.Fatal(err)
	}
	final := second["pagination"].(map[string]any)
	approvals := second["approvals"].([]any)
	if calls != 2 || final["limit"] != float64(1) || final["offset"] != float64(50) || final["total"] != float64(51) || final["next_offset"] != nil || len(approvals) != 1 || approvals[0].(map[string]any)["id"] != "approval_50" {
		t.Fatalf("continuation DTO lost: page=%v calls=%d", final, calls)
	}
}

func TestMachineMaintenanceInventoryErrorsDoNotGuessOrRetryPages(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Query().Get("limit") != "7" || r.URL.Query().Get("offset") != "123" || r.URL.Query().Get("state") != "expired" {
					t.Errorf("failed request arguments changed: %s", r.URL.RawQuery)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"code":"version_conflict","message":"fixture rejection"}}`)
			}))
			defer server.Close()
			value, err := New(server.URL, config.Credential{AccessToken: "fixture-token"}, server.Client()).MachineMaintenanceApprovals(context.Background(), "machine_fixture", 7, 123, "", "expired")
			var apiErr *APIError
			if value != nil || !errors.As(err, &apiErr) || apiErr.Status != status || calls != 1 {
				t.Fatalf("error replaced by guessed page: value=%v calls=%d err=%v", value, calls, err)
			}
		})
	}
}
