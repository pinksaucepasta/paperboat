//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestStatusProjectsSelectedWorkspaceBeforeRendering(t *testing.T) {
	_, path := localDaemonReadinessTestConfig(t, false)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" || r.Header.Get("Authorization") != "Bearer workspace-test-token" {
			t.Errorf("status did not request authenticated machine inventory")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		workspace := r.URL.Query().Get("workspace")
		if workspace == "denied" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "forbidden"}})
			return
		}
		items := []map[string]any{}
		if workspace == "personal" || workspace == "team-a" {
			items = append(items, map[string]any{"id": workspace, "alias": workspace, "installation_generation": 1})
		} else if workspace != "empty" {
			t.Errorf("status omitted selected workspace: %q", workspace)
		}
		writeAPIData(t, w, map[string]any{"items": items, "pagination": map[string]any{
			"limit": 200, "offset": 0, "total": len(items), "next_offset": nil,
		}})
	}))
	defer control.Close()
	writeWorkspaceAuthProfile(t, filepath.Dir(path), path, control.URL, "account-1")
	t.Setenv("PAPERBOAT_WORKSPACE", "")
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 7,
		ObservedAt: time.Now().UTC(), DaemonState: "ready", DaemonVersion: "dev"}
	for _, id := range []string{"personal", "team-a"} {
		snapshot.Machines = append(snapshot.Machines, localapi.MachineStatus{ID: id, Alias: id,
			RuntimeState: "ready", Generation: 3, SelectedPath: "direct", ActiveConsumers: 1,
			TransferReadiness: "ready", PreviewReadiness: "ready", SSHReadiness: "ready",
			NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown",
			PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown",
			MappingLifetime: "unknown", UpdateHealth: "healthy"})
	}
	store, err := localapi.NewSnapshotStore(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	server, err := localapi.NewServer(configuration)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	waitForCommandSocket(t, paths.SocketPath)
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	for _, workspace := range []string{"personal", "team-a", "empty", "denied"} {
		t.Run(workspace, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			args := []string{"--config", path, "--workspace", workspace, "status", "--json"}
			code := run(t.Context(), args, &out, &diagnostics)
			if workspace == "denied" {
				if code == 0 || bytes.Contains(out.Bytes(), []byte("\"machines\"")) {
					t.Fatal("denied workspace exposed a local machine snapshot")
				}
				return
			}
			var result localapi.Snapshot
			if code != 0 || json.Unmarshal(out.Bytes(), &result) != nil {
				t.Fatalf("status exit=%d output=%s diagnostics=%s", code, &out, &diagnostics)
			}
			want := 1
			if workspace == "empty" {
				want = 0
			}
			if len(result.Machines) != want || result.Generation != 7 {
				t.Fatal("status exposed another workspace or discarded local observations")
			}
			if want == 1 && (result.Machines[0].ID != workspace || result.Machines[0].ActiveConsumers != 1) {
				t.Fatal("status lost workspace identity or live transport observations")
			}
		})
	}
}
