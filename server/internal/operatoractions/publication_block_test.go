package operatoractions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Edge abuse plan 3.6.3: an operator takes one hostname down and restores it with revision checks;
// the row stays as evidence, a legal block is recorded as legal, and the owner gets one mail per block.
func TestPublicationBlockTakesDownRestoresAndMailsTheOwner(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	actor, owner, host := "operator_"+uuid.NewString(), "opa_owner_"+x, "phish-"+x+".example.test"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','operations')`, actor)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status,role,email_verified) VALUES($1,$1,$1||'@test.invalid','active','user',true)`, owner)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.publication_blocks WHERE hostname=$1`, host)
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.transactional_mail_outbox WHERE mail_id LIKE $1`, "publication-blocked:"+host+":%")
	})
	s := NewService(store)
	s.ConfigureMail(config.TransactionalMail{From: "abuse@example.test"}, true)
	if _, err := s.Execute(ctx, actor, "unblock-missing-"+x, "publication_unblock", host, "abuse_case", Input{Revision: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unblocking a name never blocked: %v", err)
	}
	if _, err := s.Execute(ctx, actor, "block-stale-"+x, "publication_block", host, "abuse_case", Input{Revision: 2}); !errors.Is(err, ErrConflict) {
		t.Fatalf("first block with revision 2: %v", err)
	}
	blocked, err := s.Execute(ctx, actor, "block-"+x, "publication_block", strings.ToUpper(host), "abuse_case", Input{Revision: 1, Legal: true})
	if err != nil || blocked != (Result{ResourceID: host, State: "blocked", Version: 1}) {
		t.Fatalf("block=%#v err=%v", blocked, err)
	}
	var legal, isBlocked bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT blocked,legal FROM paperboat.publication_blocks WHERE hostname=$1`, host).Scan(&isBlocked, &legal); err != nil || !isBlocked || !legal {
		t.Fatalf("row blocked=%v legal=%v err=%v", isBlocked, legal, err)
	}
	restored, err := s.Execute(ctx, actor, "unblock-"+x, "publication_unblock", host, "abuse_case", Input{Revision: 1})
	if err != nil || restored.State != "active" || restored.Version != 2 {
		t.Fatalf("unblock=%#v err=%v", restored, err)
	}
	// The owner is found from the serving route at block time; set it here, then block again.
	exec(`UPDATE paperboat.publication_blocks SET account_id=$2 WHERE hostname=$1`, host, owner)
	again, err := s.Execute(ctx, actor, "block-again-"+x, "publication_block", host, "abuse_case_2", Input{Revision: 2})
	if err != nil || again.State != "blocked" || again.Version != 3 {
		t.Fatalf("re-block=%#v err=%v", again, err)
	}
	var mails int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.transactional_mail_outbox WHERE mail_id=$1 AND payload->'to'->>0=$2`, "publication-blocked:"+host+":3", owner+"@test.invalid").Scan(&mails); err != nil || mails != 1 {
		t.Fatalf("owner mails=%d err=%v", mails, err)
	}
	if err = store.SQL().QueryRowContext(ctx, `SELECT legal FROM paperboat.publication_blocks WHERE hostname=$1`, host).Scan(&legal); err != nil || legal {
		t.Fatalf("a non-legal re-block kept legal=%v err=%v", legal, err)
	}
}
