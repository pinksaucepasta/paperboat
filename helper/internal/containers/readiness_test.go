package containers

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeReadinessRejectsRedirectOtherIdentityAndOversizedResponse(t *testing.T) {
	var redirected int
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(200) }))
	defer destination.Close()
	for _, scenario := range []string{"redirect", "other_node", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/selfhost/runtime-readiness" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("c", 48) {
					t.Error("readiness request lost its exact path or runtime authentication")
					w.WriteHeader(400)
					return
				}
				if scenario == "redirect" {
					http.Redirect(w, r, destination.URL, 302)
					return
				}
				id := "expected_node"
				if scenario == "other_node" {
					id = "other_node"
				}
				body, _ := json.Marshal(map[string]any{"data": map[string]any{"node_id": id, "capability": "relay", "ready": false, "server_time": time.Now().UTC()}})
				w.Write(body)
				if scenario == "oversized" {
					w.Write([]byte(strings.Repeat(" ", 16385)))
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			caPath := filepath.Join(dir, "control-ca.pem")
			if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal([]string{"-node-id", "expected_node", "-control-url", server.URL, "-control-ca", caPath})
			if err := os.WriteFile(filepath.Join(dir, "runtime.args"), args, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runtime.credential"), []byte(strings.Repeat("c", 48)), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := newNodeClient("relay", dir)
			if err != nil {
				t.Fatal(err)
			}
			defer client.http.CloseIdleConnections()
			_, err = client.readiness(context.Background())
			if err == nil {
				t.Fatal("unsafe readiness response was accepted")
			}
			if scenario == "redirect" {
				var response ResponseError
				if !errors.As(err, &response) || response.Status != 302 {
					t.Fatal("redirect lost its typed status")
				}
			}
		})
	}
	if redirected != 0 {
		t.Fatal("runtime credential followed a cross-origin redirect")
	}
}
