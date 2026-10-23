package auth

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestExistingAccountForAliasPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("aliascheck%d", time.Now().UnixNano())
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%")
	s := NewService(store, nil, FakeWorkOSVerifier{}, []string{"test-signup-key"}, true)
	existing := prefix + "@example.test"
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, prefix+"-user", existing); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, prefix+"-spelled", prefix+"+kept@other.test"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		submitted, canonical string
		exists               bool
	}{
		{prefix + "+1234@example.test", existing, true},
		{"  " + prefix + "+a+b@EXAMPLE.test ", existing, true},
		{existing, existing, false},
		{prefix + "+1234@fresh.test", prefix + "@fresh.test", false},
		{prefix + "+kept@other.test", prefix + "@other.test", false},
	} {
		canonical, exists, err := s.ExistingAccountForAlias(ctx, c.submitted)
		if err != nil || canonical != c.canonical || exists != c.exists {
			t.Fatalf("%q: canonical=%q exists=%v err=%v, want %q %v", c.submitted, canonical, exists, err, c.canonical, c.exists)
		}
	}
	if _, _, err = s.ExistingAccountForAlias(ctx, "not an address"); err == nil {
		t.Fatal("an invalid address was accepted")
	}
}
