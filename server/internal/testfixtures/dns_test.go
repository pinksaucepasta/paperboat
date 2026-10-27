package testfixtures

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

type fakeDNS struct {
	records map[string]Record
	created []Record
}

func (f *fakeDNS) Create(_ context.Context, _ string, r Record) (string, error) {
	id := strings.Repeat("a", 31) + string(rune('0'+len(f.records)))
	r.ID = id
	f.records[id] = r
	f.created = append(f.created, r)
	return id, nil
}
func (f *fakeDNS) Get(_ context.Context, _, id string) (Record, bool, error) {
	r, ok := f.records[id]
	return r, ok, nil
}
func (f *fakeDNS) Delete(_ context.Context, _, id string) error { delete(f.records, id); return nil }

func newService(dns DNS) *Service {
	return &Service{cfg: Config{Audit: audit.NewWriter(nil), Token: strings.Repeat("t", 32), Domain: "smoke.example.test", ZoneID: "zone", DNS: dns}}
}

func TestCreateRecordIsConfinedToTheTestDomain(t *testing.T) {
	dns := &fakeDNS{records: map[string]Record{}}
	s := newService(dns)
	for _, bad := range []struct{ typ, name, content string }{
		{"CNAME", "smoke.example.test", "edge.example.test"}, // the domain itself
		{"CNAME", "x.example.test", "edge.example.test"},     // sibling
		{"CNAME", "x.smoke.example.test.evil.test", "e.example.test"},
		{"CNAME", "*.smoke.example.test", "edge.example.test"}, // wildcard
		{"CNAME", "-x.smoke.example.test", "edge.example.test"},
		{"CNAME", "x..smoke.example.test", "edge.example.test"},
		{"MX", "x.smoke.example.test", "edge.example.test"}, // type
		{"CNAME", "x.smoke.example.test", "10.0.0.1"},       // CNAME to an address
		{"A", "x.smoke.example.test", "10.0.0.1"},           // private address
		{"A", "x.smoke.example.test", "::1"},
		{"TXT", "x.smoke.example.test", ""},
		{"TXT", "x.smoke.example.test", strings.Repeat("v", 256)},
	} {
		if _, err := s.CreateRecord(t.Context(), bad.typ, bad.name, bad.content); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", bad, err)
		}
	}
	if len(dns.created) != 0 {
		t.Fatalf("refused requests reached the provider: %+v", dns.created)
	}
	for _, good := range []struct{ typ, name, content string }{
		{"CNAME", "Smoke-Domain-1.Smoke.Example.Test.", "edge.example.test."},
		{"TXT", "_challenge.a.smoke.example.test", "v=token 123"},
		{"A", "x.smoke.example.test", "203.0.113.9"},
	} {
		r, err := s.CreateRecord(t.Context(), good.typ, good.name, good.content)
		if err != nil || r.ID == "" || r.Name != strings.ToLower(strings.TrimSuffix(good.name, ".")) {
			t.Errorf("%+v: %+v %v", good, r, err)
		}
	}
	if got := dns.created[0]; got.Comment != Comment || got.Type != "CNAME" {
		t.Fatalf("record not marked as a fixture: %+v", got)
	}
}

func TestGetAndDeleteOnlyTouchTheServicesOwnRecords(t *testing.T) {
	dns := &fakeDNS{records: map[string]Record{
		strings.Repeat("b", 32): {Type: "CNAME", Name: "x.smoke.example.test", Content: "e.example.test", Comment: "someone else"},
		strings.Repeat("c", 32): {Type: "CNAME", Name: "api.example.test", Content: "e.example.test", Comment: Comment},
	}}
	s := newService(dns)
	mine, err := s.CreateRecord(t.Context(), "CNAME", "x.smoke.example.test", "edge.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Record(t.Context(), mine.ID); err != nil || got.Name != "x.smoke.example.test" {
		t.Fatalf("own record: %+v %v", got, err)
	}
	for _, foreign := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)} {
		if err := s.DeleteRecord(t.Context(), foreign); !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("delete %s: %v, want ErrRecordNotFound", foreign[:4], err)
		}
		if _, ok := dns.records[foreign]; !ok && foreign != strings.Repeat("d", 32) {
			t.Errorf("a record that is not the fixtures' own was deleted: %s", foreign[:4])
		}
	}
	for _, id := range []string{"x", "../zones", strings.Repeat("A", 32)} {
		if err := s.DeleteRecord(t.Context(), id); !errors.Is(err, ErrInvalid) {
			t.Errorf("id %q: %v, want ErrInvalid", id, err)
		}
	}
	if err := s.DeleteRecord(t.Context(), mine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(t.Context(), mine.ID); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("deleted record still found: %v", err)
	}
}

func TestUnconfiguredDNSRefuses(t *testing.T) {
	s := &Service{cfg: Config{Audit: audit.NewWriter(nil), Token: strings.Repeat("t", 32)}}
	if _, err := s.Zone(); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := s.CreateRecord(t.Context(), "TXT", "x.smoke.example.test", "v"); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

func TestAuthenticateIsExactAndTheLimiterBounds(t *testing.T) {
	s, err := New(Config{Audit: audit.NewWriter(nil), Token: strings.Repeat("t", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Authenticate(strings.Repeat("t", 32)) || s.Authenticate("") || s.Authenticate(strings.Repeat("t", 31)) || s.Authenticate(strings.Repeat("u", 32)) {
		t.Fatal("authentication is not exact")
	}
	denied := 0
	for range 100 {
		if ok, _ := s.Allow(); !ok {
			denied++
		}
	}
	if denied < 50 {
		t.Fatalf("rate limit let %d of 100 immediate calls through", 100-denied)
	}
	if _, err := New(Config{Audit: audit.NewWriter(nil), Token: "short"}); err == nil {
		t.Fatal("a short token was accepted")
	}
}

func TestCloudflareClientShapeAndNoTokenInErrors(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"` + strings.Repeat("e", 32) + `"}}`))
	}))
	defer server.Close()
	cf := &Cloudflare{BaseURL: server.URL, HTTP: server.Client(), Token: func(context.Context) (string, error) { return "SECRET-TOKEN-VALUE", nil }}
	id, err := cf.Create(t.Context(), "zone1", Record{Type: "CNAME", Name: "x.smoke.example.test", Content: "e.example.test", Comment: Comment})
	if err != nil || id != strings.Repeat("e", 32) {
		t.Fatalf("create: %q %v", id, err)
	}
	if gotAuth != "Bearer SECRET-TOKEN-VALUE" || gotPath != "POST /zones/zone1/dns_records" || !strings.Contains(gotBody, `"ttl":60`) || !strings.Contains(gotBody, `"proxied":false`) {
		t.Fatalf("request: %q %q %q", gotAuth, gotPath, gotBody)
	}
	if _, found, err := cf.Get(t.Context(), "zone1", strings.Repeat("e", 32)); err != nil || found {
		t.Fatalf("a 404 must read as not found: %v %v", found, err)
	}
	bad := &Cloudflare{BaseURL: server.URL, HTTP: server.Client(), Token: func(context.Context) (string, error) { return "", errors.New("SECRET-TOKEN-VALUE leaked") }}
	if _, err := bad.Create(t.Context(), "zone1", Record{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token source error must not reach the caller: %v", err)
	}
}
