package operatorauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// F21-14 (Lane X #15): the password is checked outside the row lock, and the per-email lockout
// still trips after five failures, refuses even the right password while it lasts, and a success
// resets the count.
func TestLoginLockoutWithThePasswordCheckedOutsideTheLock(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated remote PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("lockout-%d@example.test", time.Now().UnixNano())
	password := "a sufficiently long operator password"
	encoded, err := passwordHash(password)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("op_lockout_%d", time.Now().UnixNano())
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,role,password_hash,status) VALUES($1,$2,'admin',$3,'active')`, id, email, encoded); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_sessions WHERE operator_id=$1`, id)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_accounts WHERE id=$1`, id)
	})
	s := &Service{Store: store}
	for i := range 2 {
		if _, err := s.Login(ctx, email, "wrong password"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, email, password); err != nil {
		t.Fatalf("right password after 2 failures: %v", err)
	}
	var failures int
	if err := store.SQL().QueryRowContext(ctx, `SELECT failed_attempts FROM paperboat.operator_accounts WHERE id=$1`, id).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("a success did not reset the count: %d %v", failures, err)
	}
	for i := range 5 {
		if _, err := s.Login(ctx, email, "wrong password"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if _, err := s.Login(ctx, email, password); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked account accepted the right password: %v", err)
	}
}
