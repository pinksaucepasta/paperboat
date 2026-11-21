package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// TestProbePortSelectsTheNodeTransportAndDestinationOnPostgres goes through Service.ProbePort: the
// credential picks the node, the transport and port pick the check, and only the node's allowed
// address is dialed.
func TestProbePortSelectsTheNodeTransportAndDestinationOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err := db.Migrate(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	owner := "probe_owner_" + randomIdentity()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	key := "port-probe-test-encryption-key-0123456789"
	service := New(store, key)
	node := scopedInstallationFixture(t, store, key, owner, "tunnel")
	// setIngress stores ip as the node's verified ingress address (unverified when ip is ""); the
	// registered endpoint_host is pointed somewhere else to prove it is never dialed.
	setHost := func(ip string) {
		v4, v6 := sql.NullString{}, sql.NullString{}
		if strings.Contains(ip, ":") {
			v6 = sql.NullString{String: ip, Valid: true}
		} else if ip != "" {
			v4 = sql.NullString{String: ip, Valid: true}
		}
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET endpoint_host='9.9.9.9',
public_ingress_ipv4=$1::inet,public_ingress_ipv6=$2::inet,public_ingress_verified_at=CASE WHEN $3 THEN now() END
WHERE id=$4`, v4, v6, ip != "", node.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	stun, _ := stunServer(t, stunSuccess)
	var dialed []string
	service.PortProbeDial = func(_ context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, network+" "+address)
		if network == "udp" {
			return stun, nil
		}
		_, client := net.Pipe()
		return client, nil
	}
	setHost("1.1.1.1")
	for _, test := range []struct {
		transport string
		port      int
		dial      string
	}{{"tcp", 7272, "tcp 1.1.1.1:7272"}, {"tcp", 7275, "tcp 1.1.1.1:7275"}, {"udp", 7274, "udp 1.1.1.1:7274"}} {
		dialed = nil
		got, err := service.ProbePort(ctx, node.RuntimeCredential, test.transport, test.port, "")
		if err != nil || got != (PortProbe{Reachable: true, Detail: "connected"}) || len(dialed) != 1 || dialed[0] != test.dial {
			t.Errorf("%s %d: got %+v err %v dialed %v", test.transport, test.port, got, err, dialed)
		}
	}
	dialed = nil
	if _, err := service.ProbePort(ctx, randomToken(32), "tcp", 7272, ""); !errors.Is(err, ErrUnauthorized) || len(dialed) != 0 {
		t.Errorf("unknown credential: err %v dialed %v", err, dialed)
	}
	for _, host := range []string{"", "::1", "fd00::5", "fe80::1", "127.0.0.1", "169.254.169.254"} {
		setHost(host)
		dialed = nil
		got, err := service.ProbePort(ctx, node.RuntimeCredential, "tcp", 7272, "")
		if err != nil || got != (PortProbe{Detail: "not_public"}) || len(dialed) != 0 {
			t.Errorf("%s: got %+v err %v dialed %v", host, got, err, dialed)
		}
	}
}
