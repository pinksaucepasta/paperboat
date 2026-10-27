package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/testfixtures"
)

type routeDNS struct {
	records map[string]testfixtures.Record
}

func (d *routeDNS) Create(_ context.Context, _ string, r testfixtures.Record) (string, error) {
	id := strings.Repeat("a", 32)
	d.records[id] = r
	return id, nil
}
func (d *routeDNS) Get(_ context.Context, _, id string) (testfixtures.Record, bool, error) {
	r, ok := d.records[id]
	return r, ok, nil
}
func (d *routeDNS) Delete(_ context.Context, _, id string) error { delete(d.records, id); return nil }

func TestTestFixtureRoutesAreTokenGuardedAndConfined(t *testing.T) {
	token := strings.Repeat("f", 40)
	service, err := testfixtures.New(testfixtures.Config{Audit: audit.NewWriter(nil), Token: token, Domain: "smoke.example.test", ZoneID: "zone", DNS: &routeDNS{records: map[string]testfixtures.Record{}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerTestFixtureRoutes(mux, Options{TestFixtures: service})
	call := func(method, path, body, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct{ method, path string }{{"POST", "/v1/test-fixtures/team"}, {"DELETE", "/v1/test-fixtures/team"}, {"GET", "/v1/test-fixtures/zone"}, {"POST", "/v1/test-fixtures/dns"}, {"GET", "/v1/test-fixtures/dns/" + strings.Repeat("a", 32)}, {"DELETE", "/v1/test-fixtures/dns/" + strings.Repeat("a", 32)}} {
		for _, bearer := range []string{"", "wrong-token-wrong-token-wrong-token-wrong"} {
			if w := call(c.method, c.path, `{}`, bearer); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with bearer %q: %d, want 401", c.method, c.path, bearer, w.Code)
			}
		}
	}
	if w := call("GET", "/v1/test-fixtures/zone", "", token); w.Code != 200 || !strings.Contains(w.Body.String(), `"zone":"smoke.example.test"`) {
		t.Fatalf("zone: %d %s", w.Code, w.Body)
	}
	created := call("POST", "/v1/test-fixtures/dns", `{"type":"CNAME","name":"smoke-a.smoke.example.test","content":"edge.example.test"}`, token)
	if created.Code != 200 || !strings.Contains(created.Body.String(), strings.Repeat("a", 32)) {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	if w := call("GET", "/v1/test-fixtures/dns/"+strings.Repeat("a", 32), "", token); w.Code != 200 {
		t.Fatalf("get: %d", w.Code)
	}
	if w := call("DELETE", "/v1/test-fixtures/dns/"+strings.Repeat("a", 32), "", token); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := call("GET", "/v1/test-fixtures/dns/"+strings.Repeat("a", 32), "", token); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", w.Code)
	}
	if w := call("DELETE", "/v1/test-fixtures/dns/"+strings.Repeat("a", 32), "", token); w.Code != http.StatusNoContent {
		t.Fatalf("a repeated delete must still answer 204: %d", w.Code)
	}
	for _, body := range []string{`{"type":"CNAME","name":"x.example.test","content":"edge.example.test"}`, `{"type":"MX","name":"x.smoke.example.test","content":"e.example.test"}`, `{"type":"A","name":"x.smoke.example.test","content":"10.0.0.1"}`, `{"type":"A","name":"x.smoke.example.test","content":"203.0.113.9","extra":1}`} {
		if w := call("POST", "/v1/test-fixtures/dns", body, token); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, w.Code)
		}
	}
	if w := call("DELETE", "/v1/test-fixtures/dns/not-an-id", "", token); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed record id: %d, want 400", w.Code)
	}
	if w := call("POST", "/v1/test-fixtures/team", `{"email":"not an email"}`, token); w.Code != http.StatusBadRequest {
		t.Errorf("bad email: %d, want 400", w.Code)
	}
}

func TestTestFixtureRoutesAreAbsentWithoutAToken(t *testing.T) {
	mux := http.NewServeMux()
	registerTestFixtureRoutes(mux, Options{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/test-fixtures/zone", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("%d, want 404 when no token is configured", w.Code)
	}
}
