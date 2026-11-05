package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/servernodes"
)

type operatorRequest func(method, path, body string, csrf bool, extra ...map[string]string) *httptest.ResponseRecorder

// checkServerNodeRoutes runs as a signed-in operator (TestOperatorConnectedAuthenticationDisclosureAndRevocation):
// confirmation headers, malformed input, the error shapes, a real enrollment and the list.
func checkServerNodeRoutes(t *testing.T, ctx context.Context, store *db.DB, request operatorRequest) {
	t.Helper()
	const path = "/v1/admin/server-nodes/enrollments"
	u, code, err := servernodes.NewUnenrolled("mumbai", "slot-64", "127.0.0.1", "v-test", "c-test", filepath.Join(t.TempDir(), "node.json"))
	if err != nil {
		t.Fatal(err)
	}
	node := httptest.NewUnstartedServer(u)
	node.TLS = u.TLSConfig()
	node.StartTLS()
	defer node.Close()
	addr := strings.TrimPrefix(node.URL, "https://")
	_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE id='mumbai-slot-64'`)
	defer store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE id='mumbai-slot-64'`)
	confirm := func(target string) map[string]string {
		return map[string]string{"Operator-Target": target, "Operator-Reason": "server-node-test", "Idempotency-Key": "server-node-" + target}
	}
	body := func(address, code string) string {
		b, _ := json.Marshal(map[string]string{"address": address, "code": code})
		return string(b)
	}
	expect := func(name string, r *httptest.ResponseRecorder, status int, errCode string) {
		t.Helper()
		var got struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if r.Code != status || json.Unmarshal(r.Body.Bytes(), &got) != nil || got.Error.Code != errCode || got.Error.Message == "" {
			t.Fatalf("%s: want %d %s, got %d %s", name, status, errCode, r.Code, r.Body.String())
		}
	}
	expect("unconfirmed", request("POST", path, body(addr, code), true), 400, "operator_confirmation_required")
	expect("malformed", request("POST", path, `{"address":`, true, confirm("server-nodes")), 400, "validation_failed")
	expect("unknown field", request("POST", path, `{"address":"`+addr+`","code":"x","extra":1}`, true, confirm(addr)), 400, "validation_failed")
	expect("bad code", request("POST", path, body(addr, "pbs1.short.code"), true, confirm(addr)), 400, "invalid_request")
	wrongPin := "pbs1." + strings.Repeat("A", 43) + code[strings.LastIndex(code, "."):]
	expect("wrong pin", request("POST", path, body(addr, wrongPin), true, confirm(addr)), 403, "claim_code_mismatch")
	closed := httptest.NewServer(http.NotFoundHandler())
	closedAddr := strings.TrimPrefix(closed.URL, "http://")
	closed.Close()
	expect("unreachable", request("POST", path, body(closedAddr, code), true, confirm(closedAddr)), 502, "server_node_unreachable")

	ok := request("POST", path, body(addr, code), true, confirm(addr))
	var enrolled struct {
		Data servernodes.Status `json:"data"`
	}
	if ok.Code != 200 || json.Unmarshal(ok.Body.Bytes(), &enrolled) != nil || enrolled.Data.ID != "mumbai-slot-64" || enrolled.Data.State != "registered" {
		t.Fatalf("enroll: %d %s", ok.Code, ok.Body.String())
	}
	if strings.Contains(ok.Body.String(), "dsn") || strings.Contains(ok.Body.String(), "secret") {
		t.Fatal("enrollment answer disclosed the delivered config")
	}
	expect("replayed code", request("POST", path, body(addr, code), true, confirm(addr)), 409, "server_node_exists")
	nodeAction := func(id, action string) *httptest.ResponseRecorder {
		return request("POST", "/v1/admin/server-nodes/"+id+"/"+action, `{}`, true, confirm(id))
	}
	if r := nodeAction("mumbai-slot-64", "drain"); r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"draining"`) {
		t.Fatalf("drain: %d %s", r.Code, r.Body.String())
	}
	expect("unconfirmed drain", request("POST", "/v1/admin/server-nodes/mumbai-slot-64/drain", `{}`, true), 400, "operator_confirmation_required")
	expect("unknown node", nodeAction("mumbai-slot-63", "remove"), 404, "server_node_not_found")
	listed := request("GET", "/v1/admin/server-nodes", "", false)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), `"mumbai-slot-64"`) {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.String())
	}
	if r := nodeAction("mumbai-slot-64", "remove"); r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"removed"`) {
		t.Fatalf("remove: %d %s", r.Code, r.Body.String())
	}
	expect("remove twice", nodeAction("mumbai-slot-64", "remove"), 409, "server_node_state_conflict")
}

// Every enrollment failure maps to one status and code; an infrastructure cause never hides behind a domain one.
func TestServerNodeErrorShapes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{servernodes.ErrInvalid, 400, "invalid_request"},
		{servernodes.ErrUnauthorized, 403, "claim_code_mismatch"},
		{servernodes.ErrExpired, 410, "claim_code_expired"},
		{servernodes.ErrUsed, 409, "server_node_exists"},
		{servernodes.ErrNotFound, 404, "server_node_not_found"},
		{servernodes.ErrState, 409, "server_node_state_conflict"},
		{errors.Join(servernodes.ErrUnreachable, syscall.ECONNREFUSED), 502, "server_node_unreachable"},
	} {
		request, _, _ := diagnosticRequest("/v1/admin/server-nodes/enrollments")
		response := httptest.NewRecorder()
		serverNodeError(response, request, tc.err)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
			t.Fatalf("%v: %d %s", tc.err, response.Code, response.Body.String())
		}
	}
}
