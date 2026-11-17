package connectorprotocol

import (
	"errors"
	"strings"
	"testing"
)

const snapshotRoutePrefix = `{"id":"route_1","name":"main","protocol":"http","match_type":"managed_exact","match_hostname":"demo.example.test","path_prefix":null,"origin_scheme":"http","origin_address":"127.0.0.1:3000","preserve_host":true,"host_override":null,"tls_verification":"not_applicable","tls_server_name":null,"mtls_credential_reference":null,`

func snapshotWith(route string) []byte {
	return []byte(`{"access_mode":"public","desired_state":"active","expires_at":null,"generation":1,"kind":"tunnel_config_snapshot","name":"demo","routes":[` + route + `],"schema":"paperboat.preview-tunnel/v1","stable_endpoint":"https://demo.example.test","stable_endpoint_id":"endpoint_11111111-2222-4333-8444-555555555555","tunnel_id":"tunnel_1"}`)
}

const snapshotRouteTail = `"connect_timeout_ms":10000,"idle_timeout_ms":300000,"max_concurrent_streams":1024,"desired_state":"active"}`

func TestParseConfigSnapshotAcceptsTheServersOutput(t *testing.T) {
	// the server writes tls_pin as an explicit null for a route without a pin; machines accept that and an absent key
	for _, pin := range []string{`"tls_pin":null,`, ``} {
		snapshot, err := ParseConfigSnapshot(snapshotWith(snapshotRoutePrefix+pin+snapshotRouteTail), "tunnel_1", 1)
		if err != nil || len(snapshot.Routes) != 1 || snapshot.Routes[0].TLSPin != nil {
			t.Fatalf("pin %q: %#v, %v", pin, snapshot, err)
		}
	}
}

// One validator, the strictest rules of the machine's and the edge's former ones.
func TestParseConfigSnapshotRefusals(t *testing.T) {
	tail := `"tls_pin":null,` + snapshotRouteTail
	cases := map[string][]byte{
		"match hostname with trailing dot": snapshotWith(strings.Replace(snapshotRoutePrefix, `"demo.example.test"`, `"demo.example.test."`, 1) + tail),
		"upper-case match hostname":        snapshotWith(strings.Replace(snapshotRoutePrefix, `"demo.example.test"`, `"Demo.example.test"`, 1) + tail),
		"preview lease":                    []byte(strings.Replace(string(snapshotWith(snapshotRoutePrefix+tail)), `"kind":"tunnel_config_snapshot"`, `"kind":"preview_lease"`, 1)),
		"secret member":                    []byte(strings.Replace(string(snapshotWith(snapshotRoutePrefix+tail)), `"name":"demo"`, `"name":"demo","api_token":"x"`, 1)),
		"unclean credential reference":     snapshotWith(strings.Replace(snapshotRoutePrefix, `"mtls_credential_reference":null`, `"mtls_credential_reference":"keychain://paperboat/a/../b"`, 1) + tail),
	}
	for name, payload := range cases {
		if _, err := ParseConfigSnapshot(payload, "tunnel_1", 1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	_, err := ParseConfigSnapshot([]byte(strings.Replace(string(snapshotWith(snapshotRoutePrefix+tail)), `"name":"demo"`, `"name":"demo","api_token":"x"`, 1)), "tunnel_1", 1)
	if !errors.Is(err, ErrSnapshotCredentialMaterial) || !errors.Is(err, ErrSnapshotRejected) {
		t.Errorf("secret member error = %v", err)
	}
	// an access-mode rule only the edge side had: tcp routes need a public tunnel
	private := strings.Replace(string(snapshotWith(strings.Replace(snapshotRoutePrefix, `"protocol":"http"`, `"protocol":"tcp"`, 1)+tail)), `"access_mode":"public"`, `"access_mode":"private"`, 1)
	if _, err := ParseConfigSnapshot([]byte(private), "tunnel_1", 1); err == nil {
		t.Error("tcp route on a private tunnel accepted")
	}
}
