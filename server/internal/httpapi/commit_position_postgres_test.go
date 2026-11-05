package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// A successful write answers with the primary's commit position; reads and refused writes do not; the
// position a client sends reaches the handler, and a malformed one is replaced by one no replica reaches.
func TestCommitPositionOnWritesAndClientPositionOnReadsPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
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
	defer store.Close()
	var seen string
	h := commitPosition(store, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = r.Context().Value(commitPositionKey{}).(string)
		if r.URL.Path == "/refused" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	do := func(method, path, position string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if position != "" {
			r.Header.Set(commitPositionHeader, position)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := do("POST", "/write", "").Header().Get(commitPositionHeader); !lsnPattern.MatchString(got) {
		t.Fatalf("write answered position %q", got)
	}
	if got := do("POST", "/refused", "").Header().Get(commitPositionHeader); got != "" {
		t.Fatalf("a refused write answered position %q", got)
	}
	if got := do("GET", "/read", "16/B374D848").Header().Get(commitPositionHeader); got != "" || seen != "16/B374D848" {
		t.Fatalf("read: header %q, handler saw %q", got, seen)
	}
	if do("GET", "/read", "not-an-lsn"); seen != "FFFFFFFF/FFFFFFFF" {
		t.Fatalf("a malformed position reached the handler as %q", seen)
	}
}
