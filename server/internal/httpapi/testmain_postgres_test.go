package httpapi

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Several PostgreSQL tests open the isolated database and assume its schema exists. Migrating once here means
// the test that happens to run first (-shuffle, -run subsets) is not the one that has to.
func TestMain(m *testing.M) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn != "" && db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
		if err == nil {
			err = db.Migrate(ctx, store)
			store.Close()
		}
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate the isolated test database:", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
