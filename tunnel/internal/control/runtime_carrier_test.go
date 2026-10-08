package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
)

func TestRuntimeCarrierControlRequiresCompleteBoundSnapshot(t *testing.T) {
	p := testPreviewCarrierAdmissionForHTTP(t, "preview", "operation", "runtime_route", "device.runtime.example.test", time.Now().Add(time.Hour))
	b := p.Binding
	a := RuntimeCarrierAdmission{Schema: datacarrier.RuntimeCarrierSchema, Binding: RuntimeCarrierBinding{AccountID: b.AccountID, HostID: b.HostID, TunnelID: b.TunnelID, ConnectorID: b.ConnectorID, SessionID: b.SessionID, ProcessGeneration: b.ProcessGeneration, ConfigGeneration: b.ConfigGeneration, InstallationGeneration: 1, RouteID: b.RouteID, RouteGeneration: b.RouteGeneration, EdgeNodeID: b.EdgeNodeID, EdgeProcessEpoch: b.EdgeProcessEpoch, EdgeCarrierServerSPKISHA256: b.EdgeCarrierServerSPKISHA256, EdgeCarrierServerCertificateChainPEM: b.EdgeCarrierServerCertificateChainPEM, MachineIdentityPublicKey: b.MachineIdentityPublicKey, MachineIdentityThumbprint: b.MachineIdentityThumbprint}, AttachmentGeneration: 1, ConfigContentHash: p.ConfigContentHash, EdgeEndpoints: p.EdgeEndpoints, Hostname: "device.runtime.example.test", RouteKind: datacarrier.RuntimeCarrierRoute, RouteRevision: 1, ExpiresAt: p.ExpiresAt}
	for _, scenario := range []string{"valid", "incomplete", "missing_admissions", "wrong_epoch", "wrong_kind", "wrong_key", "stale_revision", "wrong_installation"} {
		t.Run(scenario, func(t *testing.T) {
			value := a
			switch scenario {
			case "wrong_epoch":
				value.Binding.EdgeProcessEpoch = "other_epoch"
			case "wrong_kind":
				value.RouteKind = "preview_public_https_wss"
			case "wrong_key":
				value.Binding.MachineIdentityThumbprint = "sha256:wrong"
			case "stale_revision":
				value.RouteRevision++
			case "wrong_installation":
				value.Binding.InstallationGeneration = 0
			}
			body := map[string]any{"schema": datacarrier.RuntimeCarrierSchema, "complete": scenario != "incomplete", "admissions": []RuntimeCarrierAdmission{value}}
			if scenario == "missing_admissions" {
				delete(body, "admissions")
			}
			encoded, _ := json.Marshal(body)
			client := controlClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != RuntimeCarrierAdmissionsPath || r.Header.Get("Authorization") != "Bearer "+testControlCredential || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "edge_epoch_1" {
					t.Fatal("missing authenticated runtime control binding")
				}
				request, _ := io.ReadAll(r.Body)
				if string(request) != `{"edge_node_id":"edge_1","process_epoch":"edge_epoch_1"}` {
					t.Fatalf("request %s", request)
				}
				return response(http.StatusOK, string(encoded)), nil
			})
			values, err := client.RuntimeCarrierAdmissions(context.Background(), "edge_1", "edge_epoch_1")
			if scenario == "valid" {
				if err != nil || len(values) != 1 {
					t.Fatalf("valid snapshot: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}
