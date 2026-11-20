package operator

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestFailedMailIsListedAndRedrivenOncePostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
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
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	actorID, failedID, sentID := "usr_mailop_"+suffix, "mail_failed_"+suffix, "mail_sent_"+suffix
	if _, err := store.Pool().Exec(ctx, `INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$2,'unused test login','active','operations')`, actorID, actorID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO transactional_mail_outbox(mail_id,payload,state,attempts,first_attempt_at) VALUES($1,'{}','failed',8,now()-interval '24 hours'),($2,'{}','sent',1,now())`, failedID, sentID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Pool().Exec(context.Background(), `DELETE FROM transactional_mail_outbox WHERE mail_id IN($1,$2)`, failedID, sentID)
		store.Pool().Exec(context.Background(), `DELETE FROM audit_events WHERE actor_user_id=$1`, actorID)
		store.Pool().Exec(context.Background(), `DELETE FROM operator_accounts WHERE id=$1`, actorID)
	})
	service := NewService(store)
	actor := auth.User{ID: actorID, Role: auth.Role("operations")}

	value, err := service.Read(ctx, actor, "mail_failures", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	page := value.(MailFailurePage)
	var listed []string
	for _, m := range page.Mails {
		listed = append(listed, m.ID)
	}
	if page.Total < 1 || !slices.Contains(listed, failedID) || slices.Contains(listed, sentID) {
		t.Fatalf("failed-mail list = %v (total %d), want %s and not %s", listed, page.Total, failedID, sentID)
	}

	if err := service.RedriveMail(ctx, sentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("redrive of a sent mail = %v, want not found (it must never be sent twice)", err)
	}
	if err := service.RedriveMail(ctx, failedID); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	var first *time.Time
	if err := store.Pool().QueryRow(ctx, `SELECT state,attempts,first_attempt_at FROM transactional_mail_outbox WHERE mail_id=$1`, failedID).Scan(&state, &attempts, &first); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attempts != 0 || first != nil {
		t.Fatalf("redriven mail = %s, %d attempts, first attempt %v; want queued, 0, none (the 23 h clock restarts)", state, attempts, first)
	}
	if err := service.RedriveMail(ctx, failedID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second redrive = %v, want not found", err)
	}
}
