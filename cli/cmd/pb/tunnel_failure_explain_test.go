package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func failingHealth(code, dimension, dimensionCode, summary, action string, retrying bool) api.TunnelHealth {
	ready := api.TunnelHealthDimension{Status: "ready", Code: "ok"}
	health := api.TunnelHealth{Schema: api.TunnelV1Schema, Kind: "health", ResourceKind: "tunnel", ResourceID: "tun_1", OverallCode: code, Summary: summary,
		RepairAction: action, Retrying: retrying, Since: time.Unix(1, 0).UTC(),
		Dimensions: api.TunnelHealthDimensions{Service: ready, Edge: ready, Config: ready, Route: ready, Origin: ready, DNS: ready, Certificate: ready, Access: ready, Update: ready}}
	failing := api.TunnelHealthDimension{Status: "down", Code: dimensionCode}
	switch dimension {
	case "service":
		health.Dimensions.Service = failing
	case "edge":
		health.Dimensions.Edge = failing
	case "origin":
		health.Dimensions.Origin = failing
	case "dns":
		health.Dimensions.DNS = failing
	case "certificate":
		health.Dimensions.Certificate = failing
	case "route":
		health.Dimensions.Route = failing
	case "access":
		health.Dimensions.Access = failing
	}
	return health
}

// Every failure the server names must reach the user as cause + what to do in status and in doctor,
// and an empty log must say where the tunnel stands.
func TestTunnelStatusDoctorAndLogsExplainEachFailure(t *testing.T) {
	for name, health := range map[string]api.TunnelHealth{
		"connector_offline":   failingHealth("connector_offline", "service", "connector_offline", "The Paperboat host that runs this tunnel is offline or its service is stopped.", "Make sure the machine is on and online (`pb environments`), then run `pb doctor` on it.", true),
		"edge_unavailable":    failingHealth("edge_unavailable", "edge", "edge_unavailable", "The host is connected, but no Paperboat edge is ready to carry this tunnel's traffic.", "Nothing to change on your side.", true),
		"origin_unreachable":  failingHealth("origin_unreachable", "origin", "origin_refused", "The tunnel is connected, but the local app refused the connection: nothing is listening on its port.", "Start the app on the route's port.", true),
		"dns_pending":         failingHealth("dns_pending", "dns", "dns_pending", "Waiting for DNS: api.example.test is not verified yet.", "Add the DNS records shown by `pb tunnel domain instructions`.", true),
		"certificate_pending": failingHealth("certificate_pending", "certificate", "certificate_pending", "The certificate for api.example.test is still being issued.", "Wait a few minutes.", true),
		"route_unavailable":   failingHealth("route_unavailable", "route", "route_rejected", "The edge could not forward a tunnel route.", "Run `pb tunnel route list` to review the route.", true),
		"bandwidth_exhausted": failingHealth("bandwidth_exhausted", "access", "bandwidth_exhausted", "Your workspace has used its bandwidth allowance, so traffic is not forwarded.", "Add bandwidth or change plan on the Billing page.", false),
	} {
		t.Run(name, func(t *testing.T) {
			withTunnelCommandClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/status"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": health})
				case strings.HasSuffix(r.URL.Path, "/logs"):
					_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TunnelLogPage{Items: []api.TunnelLogEntry{}}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			for _, command := range []string{"status", "doctor", "logs"} {
				out, _, err := executeAdversarialTunnelCommand(t, command, "tun_1")
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				if !strings.Contains(out, health.OverallCode) || !strings.Contains(out, health.Summary) {
					t.Errorf("%s does not name the cause %q: %q", command, health.OverallCode, out)
				}
				if !strings.Contains(out, health.RepairAction) {
					t.Errorf("%s does not say what to do (%q): %q", command, health.RepairAction, out)
				}
				if command == "status" && health.Retrying != strings.Contains(out, "keeps retrying") {
					t.Errorf("status retry hint = %v, want %v: %q", strings.Contains(out, "keeps retrying"), health.Retrying, out)
				}
			}
		})
	}
}

func TestTunnelStatusStaysOneLineWhenReady(t *testing.T) {
	health := failingHealth("ready", "", "", "Tunnel is ready to accept traffic.", "No action required.", false)
	if got := tunnelStatusText(health); got != "tun_1\tready\tTunnel is ready to accept traffic." {
		t.Fatalf("ready status = %q", got)
	}
}
