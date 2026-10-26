package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A saturated database answers 503 database_busy (service_busy for an inner deadline) with Retry-After (F21-03),
// never a bare 500: a lock wait past
// lock_timeout, a statement past statement_timeout, no free server connection, or the request pool's acquire bound
// ending while the request is still live. A broken query stays a 500, and a request whose own deadline passed is
// left to the timeout middleware.
func TestSaturatedServiceAnswers503WithRetryAfter(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name   string
		cause  error
		ctx    context.Context
		status int
		code   string
	}{
		{"lock timeout", fmt.Errorf("update: %w", &pgconn.PgError{Code: "55P03"}), context.Background(), http.StatusServiceUnavailable, "database_busy"},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, context.Background(), http.StatusServiceUnavailable, "database_busy"},
		{"too many connections", &pgconn.PgError{Code: "53300"}, context.Background(), http.StatusServiceUnavailable, "database_busy"},
		{"acquire bound", fmt.Errorf("list: %w", context.DeadlineExceeded), context.Background(), http.StatusServiceUnavailable, "service_busy"},
		{"request deadline passed", context.DeadlineExceeded, expired, http.StatusInternalServerError, ""},
		{"broken query", &pgconn.PgError{Code: "42601"}, context.Background(), http.StatusInternalServerError, ""},
		{"other failure", errors.New("boom"), context.Background(), http.StatusInternalServerError, ""},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/machines", nil).WithContext(test.ctx)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", test.cause)
		busy := test.status == http.StatusServiceUnavailable
		if w.Code != test.status || (w.Header().Get("Retry-After") != "") != busy {
			t.Fatalf("%s: status %d, Retry-After %q", test.name, w.Code, w.Header().Get("Retry-After"))
		}
		if busy && !strings.Contains(w.Body.String(), `"code":"`+test.code+`"`) {
			t.Fatalf("%s: body %s", test.name, w.Body.String())
		}
	}
}
