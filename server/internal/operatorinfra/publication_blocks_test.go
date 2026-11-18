package operatorinfra

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestListPublicationBlocksPagesByHostnameAndFiltersBlocked(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	admin := "pubblock_admin_" + x
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@test.invalid','unused test login','active','operations')`, admin); err != nil {
		t.Fatal(err)
	}
	prefix := "pb" + x
	for i, blocked := range []bool{true, false, true} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.publication_blocks(hostname,blocked,legal,reason,version) VALUES($1,$2,$3,'abuse report',$4)`, fmt.Sprintf("%s-%d.example.test", prefix, i), blocked, i == 2, i+1); err != nil {
			t.Fatal(err)
		}
	}
	s := NewService(store, "operator-infrastructure-test-key-32-bytes")
	page, err := s.ListPublicationBlocks(ctx, admin, false, prefix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Hostname != prefix+"-0.example.test" || page.Items[1].Hostname != prefix+"-1.example.test" || page.NextAfter != prefix+"-1.example.test" {
		t.Fatalf("first page = %+v", page)
	}
	if page.Items[1].Blocked || page.Items[1].Version != 2 || page.Items[1].Reason != "abuse report" {
		t.Fatalf("unblocked row = %+v", page.Items[1])
	}
	next, err := s.ListPublicationBlocks(ctx, admin, false, page.NextAfter, 2)
	if err != nil || len(next.Items) == 0 || next.Items[0].Hostname != prefix+"-2.example.test" || !next.Items[0].Legal {
		t.Fatalf("second page = %+v err=%v", next, err)
	}
	blocked, err := s.ListPublicationBlocks(ctx, admin, true, prefix, 2)
	if err != nil || len(blocked.Items) != 2 || blocked.Items[1].Hostname != prefix+"-2.example.test" {
		t.Fatalf("blocked only = %+v err=%v", blocked, err)
	}
	if blocked.Total > page.Total {
		t.Fatalf("blocked total %d above all %d", blocked.Total, page.Total)
	}
	if _, err := s.ListPublicationBlocks(ctx, admin, false, "Not A Hostname", 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad cursor err = %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.operator_accounts SET role='support' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListPublicationBlocks(ctx, admin, false, "", 2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support err = %v", err)
	}
}
