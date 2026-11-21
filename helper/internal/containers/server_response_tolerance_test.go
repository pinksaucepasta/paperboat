package containers

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pbh runs for months beside a server that is deployed weekly: a field the server adds to any answer pbh reads must
// change nothing (AGENTS.md: clients ignore unknown fields). Each answer below carries one at the top level, inside
// data, and inside the nested object, for every server call pbh makes.
func TestPbhIgnoresFieldsANewerServerAddsToEveryAnswer(t *testing.T) {
	newer := map[string]any{"field_from_a_newer_server": map[string]any{"x": 1}}
	with := func(object map[string]any) map[string]any {
		object["field_from_a_newer_server"] = newer
		return object
	}
	answer := func(w http.ResponseWriter, data map[string]any) {
		_ = json.NewEncoder(w).Encode(with(map[string]any{"data": with(data)}))
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/selfhost/runtime-readiness":
			answer(w, map[string]any{"node_id": "node_1", "capability": "tunnel", "ready": false, "server_time": time.Now().UTC()})
		case "/v1/selfhost/port-probe":
			answer(w, map[string]any{"reachable": true, "detail": "echoed"})
		case "/v1/selfhost/update/check":
			answer(w, map[string]any{"update": with(map[string]any{"id": "upd_1", "target_version": "1.2.3", "state": "requested"})})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	node := filepath.Join(dir, "tunnel")
	if err := os.MkdirAll(node, 0o700); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "control-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal([]string{"-node-id", "node_1", "-control-url", server.URL, "-control-ca", caPath})
	deployment, _ := json.Marshal(map[string]string{"control_url": server.URL, "control_ca_file": caPath})
	if err := os.WriteFile(filepath.Join(node, "deployment.json"), deployment, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "runtime.args"), args, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "runtime.credential"), []byte(strings.Repeat("c", 48)), 0o600); err != nil {
		t.Fatal(err)
	}

	client, err := newNodeClient("tunnel", node)
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	if got, err := client.readiness(context.Background()); err != nil || got.NodeID != "node_1" {
		t.Fatalf("readiness with newer fields = %+v, %v", got, err)
	}
	if got, err := ProbePort(context.Background(), dir, "tcp", 8443, "nonce"); err != nil || !got.Reachable {
		t.Fatalf("port probe with newer fields = %+v, %v", got, err)
	}
	update, err := OpenUpdateNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := update.Check(context.Background(), "1.0.0"); err != nil || got == nil || got.ID != "upd_1" || got.TargetVersion != "1.2.3" {
		t.Fatalf("update check with newer fields = %+v, %v", got, err)
	}
}
