package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Session state must run on the direct pool (F21-15, PLAN 3b "PlanetScale Postgres readiness"): behind a
// transaction-pooling PgBouncer a session advisory lock's unlock, or a LISTEN, runs on another server connection,
// so the lock leaks and the listener never wakes. Every server source file that takes a session advisory lock or
// LISTENs must get its connection from the direct pool (db.DB.Direct, the migration pool built on it, or a pool
// field that the wiring fills from Direct, named here). Transaction-scoped locks (pg_advisory_xact_lock) are safe
// pooled and are not checked.
func TestSessionLocksAndListenUseTheDirectPool(t *testing.T) {
	sessionState := regexp.MustCompile(`pg_advisory_lock\(|pg_try_advisory_lock\(|pg_advisory_unlock\(|"LISTEN `)
	direct := regexp.MustCompile(`\.Direct\(\)|\.direct\.Acquire|migrations\.Conn\(|l\.DB\.Acquire`)
	// Fields filled from Direct by their wiring (checked below): workers.PostgresLeader.DB (app/lifecycle.go
	// workerLeader), machineevents.Hub.direct and mintwithdrawal.Watcher.direct (app/app.go).
	root := filepath.Join("..")
	var checked int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(raw)
		if !sessionState.MatchString(source) {
			return nil
		}
		checked++
		if !direct.MatchString(source) {
			t.Errorf("%s takes a session lock or LISTENs without a direct-pool connection", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 5 {
		t.Fatalf("checked %d files; the session-state users (migrations, team identity, machine events, tunnelcert, leader) were not found", checked)
	}
	for _, wiring := range []struct{ file, want string }{
		{"../app/lifecycle.go", "DB: store.Direct()"},
		{"../app/app.go", "machineevents.NewHub(store.Pool(), store.Direct())"},
		{"../app/app.go", "mintwithdrawal.NewWatcher(store.Pool(), store.Direct(),"},
	} {
		raw, err := os.ReadFile(wiring.file)
		if err != nil || !strings.Contains(string(raw), wiring.want) {
			t.Errorf("%s no longer wires %q (%v)", wiring.file, wiring.want, err)
		}
	}
}
