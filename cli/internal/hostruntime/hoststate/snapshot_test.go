package hoststate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func snapshotFixturePayload(generation uint64) []byte {
	return []byte(fmt.Sprintf(`{"schema":"paperboat.preview-tunnel/v1","kind":"tunnel_config_snapshot","tunnel_id":"tun_01","generation":%d,"name":"demo","desired_state":"active","access_mode":"public","stable_endpoint":"https://123e4567-e89b-12d3-a456-426614174000.tunnels.example.test","stable_endpoint_id":"endpoint_123e4567-e89b-42d3-a456-426614174000","expires_at":null,"routes":[{"id":"rte_01","name":"default","protocol":"http","match_type":"catch_all","path_prefix":null,"origin_scheme":"http","origin_address":"127.0.0.1:3000","preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"mtls_credential_reference":null,"connect_timeout_ms":10000,"idle_timeout_ms":90000,"max_concurrent_streams":128,"desired_state":"active"}]}`, generation))
}

func TestParseTunnelConfigSnapshotStrictCanonicalShape(t *testing.T) {
	valid := snapshotFixturePayload(7)
	parsed, err := ParseTunnelConfigSnapshot(valid, "tun_01", 7)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Kind != "tunnel_config_snapshot" || parsed.Generation != 7 || len(parsed.Routes) != 1 {
		t.Fatalf("parsed snapshot = %+v", parsed)
	}

	tests := []struct {
		name    string
		payload []byte
		want    error
	}{
		{name: "unknown top-level field", payload: []byte(strings.TrimSuffix(string(valid), "}") + `,"unexpected":true}`), want: ErrInvalidState},
		{name: "trailing data", payload: append(append([]byte(nil), valid...), []byte(` {}`)...), want: ErrInvalidState},
		{name: "missing required field", payload: []byte(strings.Replace(string(valid), `"name":"demo",`, "", 1)), want: ErrInvalidState},
		{name: "duplicate field", payload: []byte(strings.Replace(string(valid), `"name":"demo",`, `"name":"demo","name":"other",`, 1)), want: ErrInvalidState},
		{name: "unknown route field", payload: []byte(strings.Replace(string(valid), `"desired_state":"active"}]}`, `"desired_state":"active","unexpected":true}]}`, 1)), want: ErrInvalidState},
		{name: "unsafe credential reference", payload: []byte(strings.Replace(string(valid), `"mtls_credential_reference":null`, `"mtls_credential_reference":"https://user:secret@example.test/key"`, 1)), want: ErrCredentialMaterial},
		{name: "identity mismatch", payload: valid, want: ErrInvalidState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tunnelID, generation := "tun_01", uint64(7)
			if test.name == "identity mismatch" {
				tunnelID = "tun_02"
			}
			_, err := ParseTunnelConfigSnapshot(test.payload, tunnelID, generation)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestParseTunnelConfigSnapshotAcceptsHTTPUnixOrigin(t *testing.T) {
	payload := strings.Replace(string(snapshotFixturePayload(8)),
		`"origin_scheme":"http","origin_address":"127.0.0.1:3000"`,
		`"origin_scheme":"unix","origin_address":"/var/run/paperboat-origin.sock"`, 1)
	parsed, err := ParseTunnelConfigSnapshot([]byte(payload), "tun_01", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Routes) != 1 || parsed.Routes[0].Protocol != "http" || parsed.Routes[0].OriginScheme != "unix" {
		t.Fatalf("parsed Unix route = %+v", parsed.Routes)
	}
}

// A tunnel's config snapshot is stored byte for byte (its content hash names it) and served again until the tunnel
// changes. Every tunnel created before the route CA moved into the route ('ca_reference' became 'tls_ca') still has
// a snapshot with "ca_reference":null: the agent keeps serving those tunnels (the production canary went
// tunnel_pending when an agent refused this exact stored payload).
func TestParseTunnelConfigSnapshotAcceptsTheStoredSnapshotsOfTunnelsMadeBeforeTheRouteCAChange(t *testing.T) {
	stored := []byte(`{"access_mode":"public","desired_state":"active","expires_at":null,"generation":1,"kind":"tunnel_config_snapshot","name":"canary","routes":[{"ca_reference":null,"connect_timeout_ms":10000,"desired_state":"active","host_override":null,"id":"rte_tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03_default","idle_timeout_ms":90000,"match_hostname":"eager-purple-lantern-7514.tunnels.example.test","match_type":"managed_exact","max_concurrent_streams":1024,"mtls_credential_reference":null,"name":"default","origin_address":"127.0.0.1:18099","origin_scheme":"http","path_prefix":null,"preserve_host":true,"protocol":"http","tls_server_name":null,"tls_verification":"not_applicable"}],"schema":"paperboat.preview-tunnel/v1","stable_endpoint":"https://eager-purple-lantern-7514.tunnels.example.test","stable_endpoint_id":"endpoint_23bd453f-0558-4066-9d2e-d7e8a1b16bba","tunnel_id":"tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03"}`)
	parsed, err := ParseTunnelConfigSnapshot(stored, "tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03", 1)
	if err != nil {
		t.Fatalf("stored snapshot with ca_reference:null: %v", err)
	}
	if len(parsed.Routes) != 1 || parsed.Routes[0].OriginAddress != "127.0.0.1:18099" {
		t.Fatalf("parsed routes = %+v", parsed.Routes)
	}
	// a legacy route cannot smuggle a reference in: only the null value is accepted
	withReference := []byte(strings.Replace(string(stored), `"ca_reference":null`, `"ca_reference":"cred_x"`, 1))
	if _, err := ParseTunnelConfigSnapshot(withReference, "tunnel_f9319d66-a4ad-4b23-a342-60cb3ea6ce03", 1); err == nil {
		t.Fatal("a non-null ca_reference was accepted")
	}
}
