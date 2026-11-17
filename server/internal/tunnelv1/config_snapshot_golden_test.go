package tunnelv1

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// The content hash of a stored configuration generation is sha256 of these canonical bytes, so a change to the shared
// snapshot type must not move them for a route without a pin (tls_pin stays an explicit null). The expected document
// and hash were computed from the JSON spelled out independently of Go's encoder, matching what the server wrote before the
// type moved to connectorprotocol.
func TestInitialConfigSnapshotBytesAreStable(t *testing.T) {
	tunnel := dbsqlc.Tunnel{ID: "tunnel_1", Name: "demo", DesiredState: "active", AccessMode: "public", Generation: 1,
		StableEndpointID: "endpoint_11111111-2222-4333-8444-555555555555", StableEndpoint: "https://demo.example.test"}
	route := dbsqlc.TunnelRoute{ID: "route_1", Name: "main", Protocol: "http", MatchType: "managed", MatchHostname: sql.NullString{String: "demo.example.test", Valid: true},
		OriginScheme: "http", OriginAddress: "127.0.0.1:3000", PreserveHost: true, TlsVerification: "not_applicable",
		ConnectTimeoutMs: 10000, IdleTimeoutMs: 300000, MaxConcurrentStreams: 1024, DesiredState: "active"}
	got, err := initialConfigSnapshot(tunnel, route)
	want := `{"access_mode":"public","desired_state":"active","expires_at":null,"generation":1,"kind":"tunnel_config_snapshot","name":"demo","routes":[{"connect_timeout_ms":10000,"desired_state":"active","host_override":null,"id":"route_1","idle_timeout_ms":300000,"match_hostname":"demo.example.test","match_type":"managed_exact","max_concurrent_streams":1024,"mtls_credential_reference":null,"name":"main","origin_address":"127.0.0.1:3000","origin_scheme":"http","path_prefix":null,"preserve_host":true,"protocol":"http","tls_pin":null,"tls_server_name":null,"tls_verification":"not_applicable"}],"schema":"paperboat.preview-tunnel/v1","stable_endpoint":"https://demo.example.test","stable_endpoint_id":"endpoint_11111111-2222-4333-8444-555555555555","tunnel_id":"tunnel_1"}`
	if err != nil || string(got) != want {
		t.Fatalf("snapshot = %s, %v", got, err)
	}
	digest := sha256.Sum256(got)
	if hex.EncodeToString(digest[:]) != "28cb82301c0471416a85fff2a166ad886a5e19405af15951eac92a4678179f3c" {
		t.Fatalf("content hash moved: %x", digest)
	}
}

// The producer validates as strictly as its consumers: a snapshot a machine or edge would refuse is never stored.
func TestInitialConfigSnapshotRefusesWhatConsumersRefuse(t *testing.T) {
	tunnel := dbsqlc.Tunnel{ID: "tunnel_1", Name: "demo", DesiredState: "active", AccessMode: "public", Generation: 1,
		StableEndpointID: "not-an-endpoint-id", StableEndpoint: "https://demo.example.test"}
	route := dbsqlc.TunnelRoute{ID: "route_1", Name: "main", Protocol: "http", MatchType: "managed", MatchHostname: sql.NullString{String: "demo.example.test", Valid: true},
		OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TlsVerification: "not_applicable", ConnectTimeoutMs: 10000, IdleTimeoutMs: 300000, MaxConcurrentStreams: 1024, DesiredState: "active"}
	if _, err := initialConfigSnapshot(tunnel, route); err == nil {
		t.Fatal("a snapshot with an invalid stable endpoint id was produced")
	}
}

// The API refuses at write time what the snapshot validator would refuse at store time, so an accepted route can never
// fail its own snapshot: a path prefix over 512 bytes and a credential reference that is not a clean handle.
func TestRouteInputsAreNoLooserThanTheSnapshotValidator(t *testing.T) {
	long := "/" + strings.Repeat("a", 512)
	if _, err := normalizePathPrefix(&long); err == nil {
		t.Error("a 513-byte path_prefix was accepted")
	}
	ok := "/" + strings.Repeat("a", 511)
	if _, err := normalizePathPrefix(&ok); err != nil {
		t.Errorf("a 512-byte path_prefix was refused: %v", err)
	}
	for _, reference := range []string{"keychain://paperboat/origins/rte_1/client/", "keychain://paperboat/origins/../rte_1/client", "keychain://paperboat//origins/rte_1/client"} {
		if validateWriteOnlyReference(reference) == nil {
			t.Errorf("unclean reference %q accepted", reference)
		}
	}
	if err := validateWriteOnlyReference("keychain://paperboat/origins/rte_1/client"); err != nil {
		t.Errorf("clean reference refused: %v", err)
	}
}
