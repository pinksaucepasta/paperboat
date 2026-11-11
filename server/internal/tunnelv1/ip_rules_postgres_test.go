package tunnelv1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/accessrules"
)

// Edge abuse plan 3.5: the tunnel's manager sets and reads normalized IP lists, another account cannot
// see or change them, and empty lists remove the row.
func TestTunnelIPRulesOwnerOnlyAndNormalizedOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	owner, host := f.addAccount(t, "iprules")
	stranger, _ := f.addAccount(t, "iprules_other")
	tunnel := "tun_iprules_" + f.suffix
	f.createTunnel(t, owner, host, tunnel, "iprules-"+strings.ReplaceAll(f.suffix, "_", "-"))
	store := accessrules.New(f.database)
	got, err := store.Put(ctx, owner, "tunnel", tunnel, accessrules.Rules{Allow: []string{"198.51.100.7/24", "198.51.100.0/24", "2001:db8::1"}, Deny: []string{"::ffff:203.0.113.9"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Allow, ",") != "198.51.100.0/24,2001:db8::1/128" || strings.Join(got.Deny, ",") != "203.0.113.9/32" {
		t.Fatalf("normalized = %+v", got)
	}
	if read, err := store.Get(ctx, owner, "tunnel", tunnel); err != nil || strings.Join(read.Allow, ",") != strings.Join(got.Allow, ",") || strings.Join(read.Deny, ",") != "203.0.113.9/32" {
		t.Fatalf("read = %+v %v", read, err)
	}
	if _, err := store.Get(ctx, stranger, "tunnel", tunnel); !errors.Is(err, accessrules.ErrNotFound) {
		t.Fatalf("another account read the rules: %v", err)
	}
	if _, err := store.Put(ctx, stranger, "tunnel", tunnel, accessrules.Rules{}); !errors.Is(err, accessrules.ErrNotFound) {
		t.Fatalf("another account changed the rules: %v", err)
	}
	if _, err := store.Put(ctx, owner, "tunnel", tunnel, accessrules.Rules{Allow: []string{"bad"}}); !errors.Is(err, accessrules.ErrInvalid) {
		t.Fatalf("bad entry: %v", err)
	}
	if _, err := store.Put(ctx, owner, "tunnel", tunnel, accessrules.Rules{}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.access_ip_rules WHERE resource_kind='tunnel' AND resource_id=$1`, tunnel).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("empty lists kept a row: %d %v", rows, err)
	}
}
