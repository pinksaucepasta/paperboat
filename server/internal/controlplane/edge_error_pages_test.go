package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type savedReports struct{ saved chan supportreport.Record }

func (s savedReports) Save(_ context.Context, r supportreport.Record) error { s.saved <- r; return nil }
func (savedReports) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (savedReports) Cleanup(context.Context) error { return nil }

// A heartbeat's error pages become support_reports rows under the references the pages showed, so a visitor's
// reference resolves; a malformed entry is skipped and never stands in for another reference.
func TestEdgeErrorPagesAreStoredUnderTheirOwnReferences(t *testing.T) {
	store := savedReports{make(chan supportreport.Record, 8)}
	service, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go service.Run(runCtx)
	first, second := supportreport.NewReference(), supportreport.NewReference()
	ctx := service.Context(context.Background(), supportreport.NewReference())
	recordEdgeErrorPages(ctx, []edgeErrorPage{
		{first, 502}, {"aaa82dd9af95", 502}, {supportreport.NewReference(), 200}, {second, 404},
	})
	got := map[string]supportreport.Record{}
	for len(got) < 2 {
		select {
		case record := <-store.saved:
			got[record.Reference] = record
		case <-time.After(5 * time.Second):
			t.Fatalf("stored %d of 2 edge error pages", len(got))
		}
	}
	for reference, status := range map[string]int{first: 502, second: 404} {
		record, ok := got[reference]
		if !ok || record.Operation != "http_edge" || record.Code != "edge_error_page" || record.HTTPStatus != status {
			t.Fatalf("record for %s = %+v (found %v)", reference, record, ok)
		}
	}
	select {
	case extra := <-store.saved:
		t.Fatalf("a malformed entry was stored: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}
