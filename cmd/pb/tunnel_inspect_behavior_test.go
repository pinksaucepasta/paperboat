package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func daemonListHandler(t *testing.T, hits *atomic.Int32, grant *string, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		*grant = request.Header.Get("X-Paperboat-Inspector-Grant")
		if request.Header.Get("Authorization") != "" {
			t.Errorf("daemon request carried an Authorization bearer")
		}
		query := request.URL.Query()
		if query.Get("kind") != "tunnel" || query.Get("resource") != "tun_01" || query.Get("route") != "rte_01" {
			t.Errorf("daemon scope = %v", query)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = fmt.Fprint(writer, body)
	}
}

const inspectorTestPage = `{"schema":"paperboat.inspector/v1","kind":"capture_page","resource_id":"tun_01","records":[{"id":"cap_01","resource_id":"rte_01","method":"POST","url":"https://app.example.test/api/items","request_headers":{"Authorization":["[redacted]"]},"response_headers":{},"request_body_state":"complete","response_body_state":"complete","response_status":200,"error_code":"none","state":"complete","started_at":"2026-09-08T00:00:00Z","finished_at":"2026-09-08T00:00:01Z"}],"next_cursor":""}`

func TestTunnelInspectIssuesAsUserAndLists(t *testing.T) {
	client, fixture := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	var hits atomic.Int32
	var grant string
	inspectorTestDaemon(t, daemonListHandler(t, &hits, &grant, http.StatusOK, inspectorTestPage))
	wireInspectorTestClients(client)
	output, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "cap_01") || !strings.Contains(output, "POST") {
		t.Fatalf("output = %q", output)
	}
	if len(fixture.issued) != 1 {
		t.Fatalf("issuances = %d, want 1", len(fixture.issued))
	}
	issued := fixture.issued[0]
	if issued["resource_kind"] != "tunnel" || issued["resource_id"] != "tun_01" || issued["route_id"] != "rte_01" || issued["action"] != "inspect" {
		t.Fatalf("issuance scope = %v", issued)
	}
	if hits.Load() != 1 || grant != "grant-for-inspect" {
		t.Fatalf("daemon hits=%d grant=%q", hits.Load(), grant)
	}
}

func TestTunnelInspectRetriesOnceWithFreshIssuance(t *testing.T) {
	client, fixture := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	var hits atomic.Int32
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		if hits.Add(1) == 1 {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(writer, `{"schema":"paperboat.inspector/v1","kind":"error","code":"inspector_forbidden"}`)
			return
		}
		if request.Header.Get("X-Paperboat-Inspector-Grant") == "" {
			t.Error("retry carried no grant")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, inspectorTestPage)
	})
	wireInspectorTestClients(client)
	output, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "cap_01") {
		t.Fatalf("output = %q", output)
	}
	if len(fixture.issued) != 2 {
		t.Fatalf("issuances = %d, want exactly one retry", len(fixture.issued))
	}
	if hits.Load() != 2 {
		t.Fatalf("daemon hits=%d, want denied + retried page", hits.Load())
	}
}

func TestTunnelInspectSurfacesDoubleDenial(t *testing.T) {
	client, fixture := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	var hits atomic.Int32
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		writer.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(writer, `{"schema":"paperboat.inspector/v1","kind":"error","code":"inspector_forbidden"}`)
	})
	wireInspectorTestClients(client)
	_, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want denial", err)
	}
	if len(fixture.issued) != 2 || hits.Load() != 2 {
		t.Fatalf("issuances=%d hits=%d, want single retry then surface", len(fixture.issued), hits.Load())
	}
}

func TestTunnelInspectIssuanceDeniedContactsNoDaemon(t *testing.T) {
	client, _ := newInspectorTestServer(t, http.StatusForbidden, inspectorTestRouteJSON)
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Error("denied issuance must not contact the daemon")
	})
	wireInspectorTestClients(client)
	_, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01")
	if err == nil {
		t.Fatal("denied issuance succeeded")
	}
}

func TestTunnelInspectRequiresRouteFlagForSeveralRoutes(t *testing.T) {
	second := strings.Replace(inspectorTestRouteJSON, `"rte_01"`, `"rte_02"`, -1)
	second = strings.Replace(second, `"web"`, `"alt"`, 1)
	client, fixture := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON+","+second)
	var hits atomic.Int32
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"schema":"paperboat.inspector/v1","kind":"capture_page","resource_id":"tun_01","records":[],"next_cursor":""}`)
	})
	wireInspectorTestClients(client)
	_, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01")
	if err == nil || !strings.Contains(err.Error(), "--route") {
		t.Fatalf("err = %v, want --route guidance", err)
	}
	if hits.Load() != 0 || len(fixture.issued) != 0 {
		t.Fatalf("ambiguous target contacted daemon (%d) or issued (%d)", hits.Load(), len(fixture.issued))
	}
	output, err := runInspectorCommand(t, tunnelCobraCommandV1, "inspect", "tun_01", "--route", "rte_02", "--json")
	if err != nil {
		t.Fatal(err)
	}
	_ = output
	if len(fixture.issued) != 1 || fixture.issued[0]["route_id"] != "rte_02" {
		t.Fatalf("issuance scope = %v", fixture.issued)
	}
	if hits.Load() != 1 {
		t.Fatalf("daemon hits=%d, want 1", hits.Load())
	}
}

func TestTunnelReplayShowsSideEffects(t *testing.T) {
	client, _ := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"schema":"paperboat.inspector/v1","kind":"replay","operation_id":"replay_abc","capture_id":"cap_01","resource_id":"rte_01","method":"POST","url":"https://app.example.test/api","request_body_state":"complete","response_status":200,"response_body_state":"complete","replay_record_id":"cap_02","side_effects":"may repeat","started_at":"2026-09-08T00:00:00Z","finished_at":"2026-09-08T00:00:01Z","error_code":"none"}`)
	})
	wireInspectorTestClients(client)
	output, err := runInspectorCommand(t, tunnelCobraCommandV1, "replay", "tun_01", "cap_01", "--idempotency-key", "key_01", "--route", "rte_01")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"replay_abc", "Side effects", "key_01"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestPreviewAliasExposesSameInspectorControls(t *testing.T) {
	client, fixture := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	var hits atomic.Int32
	inspectorTestDaemon(t, func(writer http.ResponseWriter, request *http.Request) {
		hits.Add(1)
		query := request.URL.Query()
		if query.Get("resource") != "preview_01" || query.Get("route") != "preview_01" {
			t.Errorf("preview scope = %v", query)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"schema":"paperboat.inspector/v1","kind":"capture_page","resource_id":"preview_01","records":[],"next_cursor":""}`)
	})
	wireInspectorTestClients(client)
	preview := previewCobraCommandV1()
	var output strings.Builder
	preview.SetOut(&output)
	preview.SetErr(&output)
	preview.SetArgs([]string{"inspect", "preview_01"})
	if err := preview.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No captures") {
		t.Fatalf("alias output = %q", output.String())
	}
	if len(fixture.issued) != 1 || fixture.issued[0]["resource_kind"] != "preview" || fixture.issued[0]["action"] != "inspect" {
		t.Fatalf("alias issuance scope = %v", fixture.issued)
	}
	_ = hits
}

func TestPreviewAndTunnelCommandTrees(t *testing.T) {
	for path, root := range map[string]func() *cobra.Command{"tunnel": tunnelCobraCommandV1, "preview": previewCobraCommandV1} {
		for _, child := range []string{"inspect", "replay"} {
			if command, _, err := root().Find([]string{child}); err != nil || command == nil {
				t.Fatalf("%s %s missing: %v", path, child, err)
			}
		}
	}
}

func TestTunnelReplayTransportFailurePreservesGeneratedRecoveryKey(t *testing.T) {
	client, _ := newInspectorTestServer(t, http.StatusOK, inspectorTestRouteJSON)
	var hits atomic.Int32
	inspectorTestDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	wireInspectorTestClients(client)
	output, err := runInspectorCommand(t, tunnelCobraCommandV1, "replay", "tun_01", "cap_01", "--route", "rte_01", "--json")
	if err == nil || !strings.Contains(err.Error(), "--idempotency-key pb_replay_") {
		t.Fatalf("missing recovery instruction: %v", err)
	}
	if !strings.Contains(output, `"idempotency_key":"pb_replay_`) || !strings.Contains(output, `"side_effects":"unknown"`) {
		t.Fatalf("missing machine recovery result: %q", output)
	}
	if hits.Load() != 1 {
		t.Fatalf("ambiguous request retried %d times", hits.Load())
	}
}
