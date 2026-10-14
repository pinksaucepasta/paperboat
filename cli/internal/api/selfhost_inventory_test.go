package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestSelfhostInventoryExhaustsPagesWithoutWorkspaceOrDisplayFilters(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint("duplicate_", duplicate), func(t *testing.T) {
			calls, pools := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Has("workspace") || r.URL.Query().Has("capability") {
					t.Errorf("pool policy narrowed by display filters: %s", r.URL.RawQuery)
				}
				if r.URL.Path == "/v1/selfhost/pools/relay" {
					pools++
					fmt.Fprint(w, `{"data":{"mode":"self-hosted-only","installation_ids":["install_400"]}}`)
					return
				}
				if r.URL.Path != "/v1/selfhost/installations" {
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				if r.URL.Query().Get("limit") != "200" || offset != calls*200 {
					t.Errorf("unexpected page %s", r.URL.RawQuery)
				}
				calls++
				end := min(offset+200, 401)
				items := []SelfhostInstallation{}
				for i := offset; i < end; i++ {
					id := fmt.Sprintf("install_%d", i)
					if duplicate && i == 400 {
						id = "install_0"
					}
					items = append(items, SelfhostInstallation{InstallationID: id, NodeID: "shared_physical_node", Name: fmt.Sprintf("Node %d", i), ScopeKind: "account", ScopeID: "account_1", EnrollmentState: "enrolled", Capability: "relay", Ready: true})
				}
				page := SelfhostInstallationPage{Installations: items, Pagination: Pagination{Limit: 200, Offset: offset, Total: 401}, Counts: SelfhostCounts{Ready: 401, Enrolled: 401}}
				if end < 401 {
					page.Pagination.NextOffset = &end
				}
				json.NewEncoder(w).Encode(map[string]any{"data": page})
			}))
			defer server.Close()
			client := New(server.URL, config.Credential{AccessToken: "test"}, server.Client())
			if err := client.SetWorkspace("team-selected"); err != nil {
				t.Fatal(err)
			}
			items, pool, err := client.SelfhostInventory(context.Background(), "relay")
			if duplicate {
				if !errors.Is(err, ErrSelfhostInventoryInvalid) || items != nil || pools != 0 {
					t.Fatalf("invalid partial inventory used: items=%d pools=%d err=%v", len(items), pools, err)
				}
				return
			}
			if err != nil || len(items) != 401 || calls != 3 || pools != 1 || !items[400].Selected || pool.Mode != "self-hosted-only" {
				t.Fatalf("inventory truncated or policy lost: items=%d calls=%d pools=%d err=%v", len(items), calls, pools, err)
			}
		})
	}
}

func TestSelfhostInstallationPagePreservesExplicitScopeQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := url.Values{"scope_kind": {"account"}, "scope_id": {"account_exact"}, "q": {"node name"}, "capability": {"relay"}, "enrollment_state": {"pending"}, "ready": {"false"}, "limit": {"1"}, "offset": {"0"}}
		if r.URL.RawQuery != want.Encode() {
			t.Errorf("explicit scope/query changed: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"data":{"installations":[],"pagination":{"limit":1,"offset":0,"total":0,"next_offset":null},"counts":{"ready":0,"enrolled":0,"pending":0}}}`)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: "test"}, server.Client())
	if err := client.SetWorkspace("team-selected"); err != nil {
		t.Fatal(err)
	}
	_, err := client.SelfhostInstallationsPage(context.Background(), 1, 0, url.Values{"scope_kind": {"account"}, "scope_id": {"account_exact"}, "q": {"node name"}, "capability": {"relay"}, "enrollment_state": {"pending"}, "ready": {"false"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSelfhostInventoryRejectsIncompleteRequiredPagination(t *testing.T) {
	for _, data := range []string{
		`{"installations":[]}`,
		`{"installations":[],"pagination":{"limit":200,"offset":0,"total":0},"counts":{"ready":0,"enrolled":0,"pending":0}}`,
		`{"installations":[],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null},"counts":{"ready":0,"enrolled":1,"pending":0}}`,
		`{"installations":[],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":0},"counts":{"ready":0,"enrolled":1,"pending":0}}`,
		`{"installations":[],"pagination":{"limit":200,"offset":1,"total":0,"next_offset":null},"counts":{"ready":0,"enrolled":0,"pending":0}}`,
		`{"installations":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null},"counts":{"ready":1,"enrolled":0,"pending":0}}`,
	} {
		t.Run(data, func(t *testing.T) {
			poolRead := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/selfhost/installations" {
					poolRead = true
				}
				fmt.Fprintf(w, `{"data":%s}`, data)
			}))
			defer server.Close()
			items, _, err := New(server.URL, config.Credential{AccessToken: "test"}, server.Client()).SelfhostInventory(context.Background(), "relay")
			if !errors.Is(err, ErrSelfhostInventoryInvalid) || items != nil || poolRead {
				t.Fatalf("incomplete inventory treated as policy: items=%d pool=%v err=%v", len(items), poolRead, err)
			}
		})
	}
}

func TestSelfhostCanceledDiscoveryReturnsNoPartialPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, pools := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/selfhost/installations" {
			pools++
			return
		}
		calls++
		cancel()
		fmt.Fprint(w, `{"data":{"installations":[],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":0},"counts":{"ready":0,"enrolled":1,"pending":0}}}`)
	}))
	defer server.Close()
	items, _, err := New(server.URL, config.Credential{AccessToken: "test"}, server.Client()).SelfhostInventory(ctx, "relay")
	if !errors.Is(err, context.Canceled) || items != nil || calls != 1 || pools != 0 {
		t.Fatalf("canceled discovery continued: items=%d requests=%d pools=%d err=%v", len(items), calls, pools, err)
	}
}
