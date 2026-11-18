package operatoractions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A ban mails the person once, with the reference and the appeal route; a replay of the receipt and a ban by a
// person without a verified address queue nothing more.
func TestBanMailsTheBannedPersonOnce(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	s := NewService(store)
	s.ConfigureMail(config.TransactionalMail{From: "Paperboat <noreply@example.test>", AbuseMailbox: "abuse@mail.example.test"}, true)
	x := fmt.Sprint(time.Now().UnixNano())
	admin, user, unverified := "operator_"+uuid.NewString(), "ban_mail_"+x, "ban_nomail_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','x','active','admin')`, admin)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,email_verified,status,role) VALUES($1,'wo_'||$1,$1||'@example.test',true,'active','user'),($2,'wo_'||$2,$2||'@example.test',false,'active','user')`, user, unverified)
	t.Cleanup(func() {
		bg := context.Background()
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE 'account-banned:%' AND payload->>'html' LIKE '%'||$1||'%'`, x)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.bans WHERE subject_id IN ($1,$2)`, user, unverified)
		store.SQL().ExecContext(bg, `DELETE FROM paperboat.account_deletion_jobs WHERE user_id IN ($1,$2)`, user, unverified)
	})
	version := func(id string) int64 {
		var v int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT version FROM paperboat.users WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	reason := "abuse-" + x
	ban := func(id string) {
		t.Helper()
		for range 2 { // the second call replays the same receipt
			if _, err := s.Execute(ctx, admin, "ban-key-"+id, "account_ban", id, reason, Input{Revision: version(id)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	ban(user)
	ban(unverified)
	rows, err := store.SQL().QueryContext(ctx, `SELECT payload->>'subject',payload->'to'->>0,payload->>'html' FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE 'account-banned:%' AND payload->>'html' LIKE '%'||$1||'%'`, x)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var subject, to, body string
		if err := rows.Scan(&subject, &to, &body); err != nil {
			t.Fatal(err)
		}
		n++
		if to != user+"@example.test" || !strings.Contains(body, reason) || !strings.Contains(body, "abuse@mail.example.test") || !strings.Contains(body, "A person reviews every appeal") {
			t.Fatalf("ban mail to=%s subject=%q body=%s", to, subject, body)
		}
	}
	if n != 1 {
		t.Fatalf("ban mails = %d, want 1 (one verified address, replay is silent)", n)
	}
}
