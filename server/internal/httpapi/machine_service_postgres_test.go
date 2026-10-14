package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMachineServiceSignedRuntimePostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	store, e := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "ds-http-owner-"+suffix, "ds-http-machine-"+suffix, "ds-http-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$1,$1,'machine service','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	service := machineservices.New(store)
	identity := &machineServiceProof{fakeRuntimeIdentity: &fakeRuntimeIdentity{}}
	repo := &fakeRuntimeObservationRepository{}
	now := time.Now().UTC()
	snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	body, _ := json.Marshal(map[string]any{"environment_id": environment, "resource_id": machine, "sampled_at": now, "machine_services": snapshot})
	request := func(path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer current-runtime")
		r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
		w := httptest.NewRecorder()
		if path == "/v1/runtime-observations" {
			runtimeObservation(repo, identity, 10, service).ServeHTTP(w, r)
		} else {
			resolver, _ := nativeprivateaccess.NewSQLResolver(store)
			runtimeMachineServiceAuthorize(identity, &nativeprivateaccess.Service{Resolver: resolver}).ServeHTTP(w, r)
		}
		if !bytes.Equal(identity.body, body) {
			t.Fatal("runtime proof lost exact body")
		}
		return w
	}
	w := request("/v1/runtime-observations", body)
	if w.Code != 202 {
		t.Fatalf("snapshot %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Policy machineservices.RuntimePolicy `json:"machine_services_policy"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.Data.Policy.AutomaticDiscoveryEnabled || response.Data.Policy.Generation != 1 {
		t.Fatalf("discovery policy %s", w.Body.String())
	}

	// A dashboard principal has no CLI client; it must receive the same exact
	// authorized projection, without allocating browser policy rows.
	list := httptest.NewRequest(http.MethodGet, "/v1/machine-services", nil)
	list = list.WithContext(context.WithValue(list.Context(), authContextKey{}, principal{User: auth.User{ID: owner}}))
	listed := httptest.NewRecorder()
	machineServiceList(service).ServeHTTP(listed, list)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), `"browser_url":"https://`) || !strings.Contains(listed.Body.String(), "5432.office.local.pprbt.dev") {
		t.Fatalf("dashboard discovery: %d %s", listed.Code, listed.Body.String())
	}
	var policyRows int
	if e = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.lazy_access_policies WHERE machine_id=$1`, machine).Scan(&policyRows); e != nil || policyRows != 0 {
		t.Fatalf("listing allocated policies: %d %v", policyRows, e)
	}
	resolver, _ := nativeprivateaccess.NewSQLResolver(store)
	target, e := resolver.ResolveNativePrivate(ctx, nativeprivateaccess.Request{AccountID: owner, UserID: owner, CLIClientSessionID: owner, OperationID: "runtime-test", ResourceKind: "machine_service", ResourceID: machine, RouteID: "tcp:5432", Protocol: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	expected := map[string]any{"schema": "paperboat.native-private-target/v1", "resource_kind": "machine_service", "resource_id": machine, "resource_generation": target.ResourceGeneration, "route_id": target.RouteID, "route_generation": target.RouteGeneration, "target_generation": target.TargetGeneration, "owner_endpoint_id": machine, "protocol": "tcp", "target_scheme": "tcp", "target_address": target.TargetAddress, "expires_at": now.Add(5 * time.Minute), "installation_generation": target.InstallationGeneration, "boot_id": target.BootID, "policy_generation": target.PolicyGeneration, "announcement_generation": target.AnnouncementGeneration}
	body, _ = json.Marshal(map[string]any{"environment_id": environment, "resource_id": machine, "user_id": owner, "cli_client_session_id": owner, "access_session_id": target.AccessSessionID, "expected": expected})
	w = request("/v1/runtime-machine-services/authorize", body)
	if w.Code != 200 {
		t.Fatalf("authorize %d %s", w.Code, w.Body.String())
	}
	var lease struct {
		Data struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &lease) != nil || !lease.Data.ExpiresAt.After(time.Now()) || lease.Data.ExpiresAt.After(now.Add(30*time.Second)) {
		t.Fatalf("unbounded runtime lease %s", w.Body.String())
	}
	exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, owner)
	w = request("/v1/runtime-machine-services/authorize", body)
	if w.Code != 403 {
		t.Fatalf("revoked caller %d %s", w.Code, w.Body.String())
	}
}
