package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestDeviceAuthenticationDatabaseFailureIsUnavailableAndSafelyCaptured(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	handler := supportMiddleware(reports, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDeviceAuthenticationError(w, r, errors.New("SECRET_DATABASE_DETAIL"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/machines", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"authorization_unavailable"`) {
		t.Fatalf("database auth failure = %d %s", response.Code, response.Body.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("support reporter did not drain")
	}
	store.mu.Lock()
	records := append([]supportreport.Record(nil), store.records...)
	store.mu.Unlock()
	if len(records) != 1 || records[0].Code != "authorization_unavailable" || records[0].HTTPStatus != http.StatusServiceUnavailable || records[0].Cause != "service_unavailable" {
		t.Fatalf("database auth cause was not captured: %+v", records)
	}
	logOutput := logs.String()
	if strings.Contains(response.Body.String(), "SECRET_DATABASE_DETAIL") || strings.Contains(logOutput, "SECRET_DATABASE_DETAIL") {
		t.Fatal("auth failure cause text leaked into response or logs")
	}
}

func TestDeviceAuthenticationRejectStillReturnsUnauthorized(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/machines", nil)
	writeDeviceAuthenticationError(recorder, request, auth.ErrUnauthenticated)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("invalid token = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkOSCallbackRequiresExactTrustedOriginBeforeProcessing(t *testing.T) {
	service := auth.NewService(nil, nil, nil, []string{"test-key"}, true, "https://login.example.test")
	handler := workOSCallback(service)
	for _, tc := range []struct {
		name   string
		origin string
		code   int
	}{
		{name: "missing", code: http.StatusForbidden},
		{name: "different", origin: "https://evil.example", code: http.StatusForbidden},
		{name: "exact reaches body validation", origin: "https://login.example.test", code: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", strings.NewReader("not-json"))
			request.Header.Set("Origin", tc.origin)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tc.code {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRequireAuthRejectsUnsafeRequestWithoutTrustedOrigin(t *testing.T) {
	service := auth.NewService(nil, nil, nil, nil, true, "https://login.example.test")
	handler := requireAuth(service, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler ran")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/resource", nil))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"origin_failed"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestWorkOSStateLeavesConsoleNavigationToConsole(t *testing.T) {
	service := auth.NewService(nil, nil, nil, []string{"test-key"}, false)
	handler := workOSState(service)
	for _, target := range []string{"/cli/authorize?code=ABCD", "//attacker.example", "https://attacker.example/"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/auth/workos/state?return_to="+url.QueryEscape(target), nil)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || len(recorder.Result().Cookies()) != 1 {
			t.Fatalf("state issuance unexpectedly owns console navigation: status=%d", recorder.Code)
		}
		var response struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data) != 1 || response.Data["state"] == "" {
			t.Fatal("state response must contain only login state")
		}
		validate := httptest.NewRequest(http.MethodPost, "/v1/auth/workos/callback", nil)
		validate.AddCookie(recorder.Result().Cookies()[0])
		if err := service.ValidateOAuthState(validate, response.Data["state"]); err != nil {
			t.Fatal(err)
		}
	}
}
