package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

func TestWorkOSSignupEmailPolicyPostgres(t *testing.T) {
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
	prefix := fmt.Sprintf("emailpolicy%d", time.Now().UnixNano())
	defer store.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE workos_subject LIKE $1`, prefix+"%")
	s := NewService(store, nil, FakeWorkOSVerifier{}, []string{"test-signup-key"}, true)
	login := func(subject, email string) (User, Session, error) {
		return s.VerifyCallback(ctx, CallbackInput{Code: prefix + subject + ":" + email})
	}
	if _, session, e := login("plus", prefix+"+tag@example.test"); !errors.Is(e, emailpolicy.ErrPlusAddress) || session.Token != "" {
		t.Fatal("plus-address signup created an account/session")
	}
	first, _, err := login("first", strings.ToUpper(prefix)+".One@GMAIL.COM")
	if err != nil {
		t.Fatal(err)
	}
	var canonical string
	if err = store.SQL().QueryRowContext(ctx, `SELECT canonical_email FROM paperboat.users WHERE id=$1`, first.ID).Scan(&canonical); err != nil || canonical != prefix+"one@gmail.com" {
		t.Fatalf("canonical identity not stored: %v", err)
	}
	if _, session, e := login("duplicate", prefix+"one@googlemail.com"); !errors.Is(e, emailpolicy.ErrDuplicate) || session.Token != "" {
		t.Fatal("canonical duplicate created an account/session")
	}
	if again, _, e := login("first", strings.ToUpper(prefix)+".One@GMAIL.COM"); e != nil || again.ID != first.ID {
		t.Fatalf("existing identity cannot sign in: %v", e)
	}
	legacy := prefix + "legacy"
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, legacy, prefix+".Old+tag@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = login("legacy", prefix+".Old+tag@gmail.com"); err != nil {
		t.Fatalf("existing plus-address identity blocked: %v", err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.users SET primary_email=$2 WHERE id=$1`, legacy, prefix+".Old@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = login("legacyduplicate", prefix+"old@googlemail.com"); err == nil {
		t.Fatal("canonical legacy duplicate admitted")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, e := login(fmt.Sprintf("race%d", i), prefix+"race@example.test")
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, emailpolicy.ErrDuplicate) {
			t.Fatalf("concurrent signup returned non-policy error: %v", e)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent canonical signups admitted %d accounts", success)
	}
	var rejected int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.users WHERE workos_subject IN ($1,$2,$3)`, prefix+"plus", prefix+"duplicate", prefix+"legacyduplicate").Scan(&rejected); err != nil || rejected != 0 {
		t.Fatalf("rejected signup left account rows: %v", err)
	}
}
