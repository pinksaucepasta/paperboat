package control

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

func TestRuntimeCarrierControlRequiresCompleteBoundSnapshot(t *testing.T) {
	publicKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for index := range publicKey {
		publicKey[index] = byte(index + 1)
	}
	digest := sha256.Sum256(publicKey)
	trust, err := NewProcessCarrierServerTrust("edge_1", "edge_epoch_1", "edge.example.test", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := runtimewire.Admission{Schema: runtimewire.Schema, Binding: runtimewire.Binding{AccountID: "account_1", HostID: "host_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "session_1", ProcessGeneration: 1, ConfigGeneration: 1, InstallationGeneration: 1, RouteID: "runtime_route", RouteGeneration: 1, EdgeNodeID: "edge_1", EdgeProcessEpoch: "edge_epoch_1", EdgeCarrierServerSPKISHA256: trust.SPKISHA256, EdgeCarrierServerCertificateChainPEM: trust.CertificateChainPEM, MachineIdentityPublicKey: base64.RawURLEncoding.EncodeToString(publicKey), MachineIdentityThumbprint: "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])}, AttachmentGeneration: 1, ConfigContentHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", EdgeEndpoints: []string{"h2://edge.example.test:27443", "h3://edge.example.test:27444"}, Hostname: "machine.runtime.example.test", RouteKind: runtimewire.RouteKind, RouteRevision: 1, ExpiresAt: time.Now().Add(time.Hour)}
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
			body := map[string]any{"schema": runtimewire.Schema, "complete": scenario != "incomplete", "admissions": []runtimewire.Admission{value}}
			if scenario == "missing_admissions" {
				delete(body, "admissions")
			}
			encoded, _ := json.Marshal(body)
			client := controlClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != runtimewire.EdgePullPath || r.Header.Get("Authorization") != "Bearer "+testControlCredential || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "edge_epoch_1" {
					t.Fatal("missing authenticated runtime control binding")
				}
				request, _ := io.ReadAll(r.Body)
				if string(request) != `{"edge_node_id":"edge_1","process_epoch":"edge_epoch_1","capabilities":["endpoints","tunnel_endpoints","endpoint_deltas"]}` {
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

// Production 2026-11-05: a host's policy change replaced its runtime carrier just before the edge
// reported it, and the server answered 409 runtime_carrier_stale after storing the rest. That is
// the protocol's expected answer, so it must not record a control failure; a 503 still does.
func TestRuntimeCarrierStaleObservationIsNotAControlFailure(t *testing.T) {
	status := http.StatusConflict
	var failures []error
	var outcomes []string
	client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != runtimewire.EdgeObservePath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		return response(status, `{"error":{"code":"runtime_carrier_stale"}}`), nil
	})}, ControlTrace: func(context.Context, string) (string, string, func(string, string)) {
		return "", "", func(outcome, code string) { outcomes = append(outcomes, outcome+":"+code) }
	}, ControlFailure: func(_ context.Context, _ string, err error) { failures = append(failures, err) }})
	if err != nil {
		t.Fatal(err)
	}
	observations := []runtimewire.Observation{{RouteID: "runtime_route", SessionID: "session_1", AttachmentGeneration: 1, Ready: true}}
	if err := client.ObserveRuntimeCarriers(context.Background(), "edge_1", "edge_epoch_1", observations); !errors.Is(err, ErrRuntimeCarrierStale) {
		t.Fatalf("stale observation error = %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("stale observation recorded control failures: %v", failures)
	}
	status = http.StatusServiceUnavailable
	if err := client.ObserveRuntimeCarriers(context.Background(), "edge_1", "edge_epoch_1", observations); errors.Is(err, ErrRuntimeCarrierStale) || err == nil {
		t.Fatalf("unavailable observation error = %v", err)
	}
	if len(failures) != 1 || strings.Join(outcomes, ",") != "rejected:invalid,failed:control_request_failed" {
		t.Fatalf("outcomes = %v, failures = %d", outcomes, len(failures))
	}
}
