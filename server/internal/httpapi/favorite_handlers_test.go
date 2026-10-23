package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/favorites"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestFavoriteSetRejectsIncompleteOrNonSchemaJSON(t *testing.T) {
	valid := `{"kind":"machine","resource_id":"mch_test","favorite":true}`
	tests := []struct {
		name string
		body string
	}{
		{name: "missing required favorite flag", body: `{"kind":"machine","resource_id":"mch_test"}`},
		{name: "unknown field", body: `{"kind":"machine","resource_id":"mch_test","favorite":true,"admin":true}`},
		{name: "second JSON value", body: valid + ` {}`},
		{name: "body over limit", body: valid + strings.Repeat(" ", 8192)},
	}
	handler := favoriteSet(favorites.NewService(nil))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v1/favorites", strings.NewReader(test.body))
			ctx := context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_favorite_test", Role: auth.RoleUser}})
			request = request.WithContext(ctx)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDecodeFavoriteSetRequestPreservesExplicitTrueAndFalse(t *testing.T) {
	for _, favorite := range []bool{true, false} {
		t.Run(map[bool]string{true: "true", false: "false"}[favorite], func(t *testing.T) {
			body := `{"kind":"machine","resource_id":"mch_test","favorite":` + map[bool]string{true: "true", false: "false"}[favorite] + `}`
			request := httptest.NewRequest(http.MethodPut, "/v1/favorites", strings.NewReader(body))
			response := httptest.NewRecorder()
			decoded, ok := decodeFavoriteSetRequest(response, request)
			if !ok || decoded.Favorite == nil || *decoded.Favorite != favorite || decoded.Kind != "machine" || decoded.ResourceID != "mch_test" {
				t.Fatalf("decoded=%+v ok=%v response=%s", decoded, ok, response.Body.String())
			}
		})
	}
}

func TestFavoriteSetErrorMappingRequiresPureExpectedCause(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "limit", err: favorites.ErrLimit, wantStatus: http.StatusConflict, wantCode: "favorite_limit_reached"},
		{name: "resource missing", err: favorites.ErrResourceNotFound, wantStatus: http.StatusNotFound, wantCode: "favorite_resource_not_found"},
		{name: "invalid workspace", err: workspace.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_workspace"},
		{name: "workspace forbidden", err: workspace.ErrForbidden, wantStatus: http.StatusForbidden, wantCode: "workspace_unavailable"},
		{name: "invalid favorite", err: favorites.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_favorite"},
		{name: "limit with database error", err: errors.Join(favorites.ErrLimit, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
		{name: "missing resource with database error", err: errors.Join(favorites.ErrResourceNotFound, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
		{name: "invalid workspace with database error", err: errors.Join(workspace.ErrInvalid, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
		{name: "workspace denial with database error", err: errors.Join(workspace.ErrForbidden, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
		{name: "invalid favorite with database error", err: errors.Join(favorites.ErrInvalid, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v1/favorites", nil)
			response := httptest.NewRecorder()
			writeFavoriteSetError(response, request, test.err)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status=%d body=%s, want status=%d code=%q", response.Code, response.Body.String(), test.wantStatus, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "input/output error") {
				t.Fatalf("operational cause leaked in response: %s", response.Body.String())
			}
		})
	}
}

func TestFavoriteListErrorMappingRequiresPureExpectedCause(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid workspace", err: workspace.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_workspace"},
		{name: "workspace forbidden", err: workspace.ErrForbidden, wantStatus: http.StatusForbidden, wantCode: "workspace_unavailable"},
		{name: "workspace denial with database error", err: errors.Join(workspace.ErrForbidden, syscall.EIO), wantStatus: http.StatusInternalServerError, wantCode: "internal_error"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/favorites", nil)
			response := httptest.NewRecorder()
			writeFavoriteListError(response, request, test.err)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status=%d body=%s, want status=%d code=%q", response.Code, response.Body.String(), test.wantStatus, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "input/output error") {
				t.Fatalf("operational cause leaked in response: %s", response.Body.String())
			}
		})
	}
}

func TestFavoriteMixedFailureRetainsCauseAtHTTPFinalOwner(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	reference := supportreport.NewReference()
	request := httptest.NewRequest(http.MethodPut, "/v1/favorites", nil)
	request.Header.Set("Support-Reference", reference)
	response := httptest.NewRecorder()
	mixed := errors.Join(favorites.ErrResourceNotFound, syscall.EIO)
	handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeFavoriteSetError(w, r, mixed)
	}))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "input/output error") {
		t.Fatalf("mixed failure response=%d %s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) != 0 {
			if len(records) != 1 {
				t.Fatalf("captured %d failures, want one", len(records))
			}
			record := records[0]
			if record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusInternalServerError || record.Reference != reference || record.Outcome != "failed" {
				t.Fatalf("HTTP final owner lost mixed failure cause or reference: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("HTTP final owner did not capture the operational cause")
}
