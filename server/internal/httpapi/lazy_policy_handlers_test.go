package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/lazyaccess"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

type lazyPolicyStub struct {
	LazyPolicyAPI
	calls   int
	account string
	err     error
}

func (s *lazyPolicyStub) Upsert(_ context.Context, account string, in lazyaccess.UpsertRequest) (lazyaccess.Policy, error) {
	s.calls++
	s.account = account
	return lazyaccess.Policy{ID: "policy_fixture"}, s.err
}
func TestLazyPolicyPrincipalAndBoundedJSON(t *testing.T) {
	s := &lazyPolicyStub{}
	mux := http.NewServeMux()
	pass := func(h http.Handler) http.Handler { return h }
	registerLazyPolicyRoutes(mux, s, pass, pass)
	for _, tc := range []struct {
		body          string
		authenticated bool
		status        int
	}{
		{`{}`, false, 401},
		{`{"account_id":"victim"}`, true, 400},
		{`{"machine_id":"a","machine_id":"b"}`, true, 400},
		{`{"machine_id":"` + strings.Repeat("a", 17<<10) + `"}`, true, 400},
		{`{"machine_id":"machine_fixture","target":{"scheme":"http","address":"127.0.0.1:3000"},"access_mode":"private"}`, true, 200},
	} {
		r := httptest.NewRequest(http.MethodPut, "/v1/lazy-policies", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.authenticated {
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "owner_fixture"}}))
		}
		w := httptest.NewRecorder()
		before := s.calls
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d", w.Code, tc.status)
		}
		if tc.status != 200 && before != s.calls {
			t.Fatal("invalid request reached policy store")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("policy response must not be cached")
		}
	}
	if s.calls != 1 || s.account != "owner_fixture" {
		t.Fatal("owner identity not taken from authenticated principal")
	}
}

func TestLazyPolicyOperationalFailuresKeepCauseAndRecover(t *testing.T) {
	const private = "LAZY_POLICY_PRIVATE"
	pg := &pgconn.PgError{Code: "40001", Message: private}
	cases := []struct {
		name   string
		cause  error
		status int
	}{
		{"denied", lazyaccess.ErrDenied, 404},
		{"mixed denied", errors.Join(lazyaccess.ErrDenied, syscall.EIO, errors.New(private)), 503},
		{"conflict", lazyaccess.ErrConflict, 409},
		{"sql conflict", errors.Join(lazyaccess.ErrConflict, pg), 409},
		{"mixed sql conflict", errors.Join(lazyaccess.ErrConflict, pg, syscall.EIO), 503},
		{"capacity", lazyaccess.ErrCapacity, 429},
		{"mixed capacity", errors.Join(lazyaccess.ErrCapacity, syscall.EIO, errors.New(private)), 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &lazyPolicyStub{err: tc.cause}
			mux := http.NewServeMux()
			pass := func(h http.Handler) http.Handler { return h }
			registerLazyPolicyRoutes(mux, service, pass, pass)
			request, logs, reference := diagnosticRequest("/v1/lazy-policies")
			request.Method = http.MethodPut
			request.Header.Set("Content-Type", "application/json")
			request.Body = io.NopCloser(strings.NewReader(`{}`))
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "owner_fixture"}}))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d", response.Code, tc.status)
			}
			if strings.Contains(response.Body.String()+logs.String(), private) {
				t.Fatal("private database/cause text disclosed")
			}
			if tc.status == 503 {
				var event map[string]any
				if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
					t.Fatalf("single fault missing: %v", err)
				}
				if event["errno"] != float64(syscall.EIO) || event["support_reference"] != reference {
					t.Fatalf("fault lost original cause/reference: %v", event)
				}
			}
			service.err = nil
			healthy := httptest.NewRequest(http.MethodPut, "/v1/lazy-policies", strings.NewReader(`{}`))
			healthy.Header.Set("Content-Type", "application/json")
			healthy = healthy.WithContext(context.WithValue(healthy.Context(), authContextKey{}, principal{User: auth.User{ID: "owner_fixture"}}))
			response = httptest.NewRecorder()
			mux.ServeHTTP(response, healthy)
			if response.Code != 200 || service.calls != 2 {
				t.Fatalf("same API did not recover: status=%d calls=%d", response.Code, service.calls)
			}
		})
	}
}
