package teams

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// TestRoleMatrixMatchesSQL keeps paperboat.team_role_allows equal to Allowed.
func TestRoleMatrixMatchesSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	for _, role := range append(Roles, "stranger") {
		for _, p := range append(Permissions, "unknown") {
			var got bool
			if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.team_role_allows($1,$2)`, role, string(p)).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := Allowed(role, p); got != want {
				t.Errorf("SQL team_role_allows(%q, %q) = %v, Go = %v", role, p, got, want)
			}
		}
	}
}
