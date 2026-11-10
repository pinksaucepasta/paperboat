package supportreport

import (
	"context"
	"testing"
)

// An edge error page's reference is stored as given, so the lookup that finds any support reference finds it.
func TestRecordReferencedStoresTheRefusalUnderTheGivenReference(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reference := NewReference()
	s.RecordReferenced(context.Background(), reference, "http_edge", "edge_error_page", 503)
	s.process(context.Background(), <-s.queue)
	if len(store.records) != 1 {
		t.Fatalf("stored %d records", len(store.records))
	}
	got := store.records[0]
	if got.Reference != reference || got.Operation != "http_edge" || got.Code != "edge_error_page" || got.HTTPStatus != 503 || got.Outcome != "rejected" {
		t.Fatalf("record = %+v", got)
	}
}

func TestRecordReferencedIgnoresAReferenceThatIsNotOne(t *testing.T) {
	store := &memoryStore{}
	s, err := New(store, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, bad := range []string{"", "aaa82dd9af95", "support_not-a-uuid"} {
		s.RecordReferenced(context.Background(), bad, "http_edge", "edge_error_page", 502)
	}
	if len(s.queue) != 0 {
		t.Fatalf("%d records queued for invalid references", len(s.queue))
	}
}
