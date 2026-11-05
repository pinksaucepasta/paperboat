package servernodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func startUnenrolled(t *testing.T, instance string) (*Unenrolled, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node-config.json")
	u, code, err := NewUnenrolled("mumbai", instance, "127.0.0.1", "v-test", "c-test", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(u)
	srv.TLS = u.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return u, code, strings.TrimPrefix(srv.URL, "https://")
}

// An operator claims a fresh node with its code: the node is recorded as registered, and only the node can
// read the delivered config (sealed to its key), which carries the shared database and its own identity.
// A replayed code, a wrong pin and an already used region/instance are refused.
func TestServerNodeEnrollmentDeliversSealedConfigOnPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	slot := fmt.Sprintf("slot-%02d", 1+time.Now().UnixNano()%64)
	id := "mumbai-" + slot
	cleanup := func() { _, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE id=$1`, id) }
	cleanup()
	t.Cleanup(cleanup)

	base := config.Default()
	base.Database.DSN = "postgres://primary.example.test/paperboat"
	base.Reporting.Region, base.Reporting.Instance = "helsinki", "slot-01"
	e := &Enroller{DB: store, Config: base, Dial: func(_ context.Context, a string) (string, error) { return a, nil }}

	u, code, addr := startUnenrolled(t, slot)
	pin, _, _ := parseCode(code)
	wrongPin := codePrefix + "." + b64(make([]byte, 32)) + code[len(codePrefix)+1+len(b64(pin)):]
	if _, err := e.Enroll(ctx, "operator-test", addr, wrongPin); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a code with the wrong pin was accepted: %v", err)
	}
	got, err := e.Enroll(ctx, "operator-test", addr, code)
	if err != nil || got.ID != id || got.State != "registered" {
		t.Fatalf("enroll: %+v %v", got, err)
	}
	select {
	case <-u.Claimed():
	default:
		t.Fatal("node not marked claimed")
	}
	info, err := os.Stat(u.BundlePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle not stored owner-only: %v %v", info, err)
	}
	raw, _ := os.ReadFile(u.BundlePath)
	cfg := config.Default()
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Database.DSN != base.Database.DSN || cfg.Reporting.Region != "mumbai" || cfg.Reporting.Instance != slot {
		t.Fatalf("bundle config: region %q instance %q dsn ok %v err %v", cfg.Reporting.Region, cfg.Reporting.Instance, cfg.Database.DSN == base.Database.DSN, err)
	}
	var state, host string
	var spki []byte
	if err := store.Pool().QueryRow(ctx, `SELECT state, endpoint_host, tls_spki_sha256 FROM paperboat.control_server_nodes WHERE id=$1`, id).Scan(&state, &host, &spki); err != nil || state != "registered" || host != "127.0.0.1" || string(spki) != string(pin) {
		t.Fatalf("row: %s %s %v", state, host, err)
	}
	if _, err := e.Enroll(ctx, "operator-test", addr, code); !errors.Is(err, ErrUsed) {
		t.Fatalf("a claimed code was accepted again: %v", err)
	}
	_, code2, addr2 := startUnenrolled(t, slot)
	if _, err := e.Enroll(ctx, "operator-test", addr2, code2); !errors.Is(err, ErrUsed) {
		t.Fatalf("a second node took an enrolled region/instance: %v", err)
	}
}
