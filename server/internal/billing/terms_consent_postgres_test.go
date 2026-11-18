package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/legalconsent"
)

// The record names who accepted which wording and when, and a retry of the same operation keeps the first one.
func TestRecordTermsKeepsTheFirstAcceptance(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	key, actor := "terms-op-"+suffix, "usr_terms_"+suffix
	t.Cleanup(func() {
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_terms_consents WHERE operation_id=$1`, key)
	})
	s := billing.NewWorkspaceService(store, nil, "")
	if err := s.RecordTerms(ctx, key, actor, "checkout"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTerms(ctx, key, "someone_else", "topup"); err != nil {
		t.Fatal(err)
	}
	var who, kind, version, digest string
	var at time.Time
	if err := store.SQL().QueryRowContext(ctx, `SELECT actor_user_id,kind,terms_version,text_sha256,accepted_at FROM paperboat.billing_terms_consents WHERE operation_id=$1`, key).Scan(&who, &kind, &version, &digest, &at); err != nil {
		t.Fatal(err)
	}
	if who != actor || kind != "checkout" || version != legalconsent.Version || digest != legalconsent.TextSHA256() || time.Since(at) > time.Minute {
		t.Fatalf("record = %s %s %s %s %v", who, kind, version, digest, at)
	}
	if err := s.RecordTerms(ctx, key+"-x", actor, "refund"); err == nil {
		t.Fatal("an unknown purchase kind must be refused by the table")
	}
}
