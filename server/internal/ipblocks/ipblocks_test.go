package ipblocks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestParseMasksAndBoundsRanges(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":         "203.0.113.9/32",
		"203.0.113.77/24":     "203.0.113.0/24",
		"::ffff:203.0.113.9":  "203.0.113.9/32",
		"2001:db8:77:5::1":    "2001:db8:77:5::1/128",
		"2001:db8:77:5::1/48": "2001:db8:77::/48",
		"  198.51.100.0/8 ":   "198.0.0.0/8",
		"2000::/16":           "2000::/16",
	} {
		got, err := Parse(in)
		if err != nil || got.String() != want {
			t.Fatalf("Parse(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "banana", "0.0.0.0/0", "10.0.0.0/7", "::/0", "2000::/15", "203.0.113.9%eth0", "203.0.113.0/33"} {
		if _, err := Parse(in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Parse(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

func TestBlocksMatchRangesExpireAndLiftPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) // the first cleanup runs last: later cleanups still use store
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	reason := fmt.Sprint("ipblocks", time.Now().UnixNano())
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.ip_blocks WHERE reason=$1`, reason)
	})
	add := func(cidr string, ends *time.Time) Block {
		t.Helper()
		prefix, err := Parse(cidr)
		if err != nil {
			t.Fatal(err)
		}
		var b Block
		if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			b, err = AddTx(ctx, tx, "operator_x", prefix, reason, ends)
			return
		}); err != nil {
			t.Fatalf("add %s: %v", cidr, err)
		}
		return b
	}
	blocked := func(ip string) bool {
		t.Helper()
		err := Check(ctx, store.Pool(), ip)
		if err != nil && !errors.Is(err, ErrBlocked) {
			t.Fatal(err)
		}
		return err != nil
	}
	v4 := add("203.0.113.0/24", nil)
	add("2001:db8:77::/48", nil)
	add("198.51.100.7", nil)
	for ip, want := range map[string]bool{
		"203.0.113.1": true, "203.0.113.254": true, "203.0.114.1": false, "::ffff:203.0.113.9": true,
		"2001:db8:77::1": true, "2001:db8:77:ffff::9": true, "2001:db8:78::1": false, "2001:DB8:77::2": true,
		"198.51.100.7": true, "198.51.100.8": false, "": false, "not-an-ip": false, "fe80::1%eth0": false,
	} {
		if got := blocked(ip); got != want {
			t.Fatalf("blocked(%q) = %v, want %v", ip, got, want)
		}
	}

	// A block that ran out refuses nothing and is replaced by a new block of the same range; a running one is ErrExists.
	prefix, _ := Parse("203.0.113.0/24")
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := AddTx(ctx, tx, "operator_x", prefix, reason, nil)
		return err
	}); !errors.Is(err, ErrExists) {
		t.Fatalf("a duplicate: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.ip_blocks SET expires_at=now()-interval '1 second' WHERE id=$1`, v4.ID); err != nil {
		t.Fatal(err)
	}
	if blocked("203.0.113.1") {
		t.Fatal("an expired block still refuses")
	}
	soon := time.Now().Add(time.Hour)
	again := add("203.0.113.0/24", &soon)
	if !blocked("203.0.113.1") || again.ID == v4.ID {
		t.Fatal("the replacement does not refuse")
	}
	past := time.Now().Add(-time.Minute)
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := AddTx(ctx, tx, "operator_x", prefix, reason, &past)
		return err
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a past expiry: %v", err)
	}

	// Lifting ends it at once; lifting twice, or an ended block, is not found.
	if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := RemoveTx(ctx, tx, "operator_x", again.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if blocked("203.0.113.1") {
		t.Fatal("a lifted block still refuses")
	}
	for _, id := range []string{again.ID, v4.ID, "ipb_missing"} {
		if err = store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { _, err := RemoveTx(ctx, tx, "operator_x", id); return err }); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lifting %s: %v", id, err)
		}
	}
}
