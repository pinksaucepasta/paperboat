package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

func TestSelfhostOperationalFailureKeepsCauseAndCorrelation(t *testing.T) {
	store := openSelfhostClosedPostgres(t)
	request, logs, reference := diagnosticRequest("/v1/network/selfhost/installations?scope_kind=account&scope_id=account_fault_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_fault_01"}}))
	response := httptest.NewRecorder()
	(&SelfhostHandlers{Service: selfhost.New(store, strings.Repeat("k", 32))}).Account(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "selfhost_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("response omitted safe failure identity: %+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode single diagnostic event: %v (%s)", err, logs.String())
	}
	for key, want := range map[string]any{
		"operation":         "http_network",
		"stage":             "http_handler",
		"code":              "selfhost_unavailable",
		"cause":             "connection_refused",
		"errno":             float64(syscall.ECONNREFUSED),
		"http_status":       float64(http.StatusServiceUnavailable),
		"support_reference": reference,
		"correlation_id":    "correlation_fault_01",
		"outcome":           "failed",
	} {
		if event[key] != want {
			t.Errorf("event %s=%v, want %v: %s", key, event[key], want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "SELFHOST_HTTP_PASSWORD_CANARY") || strings.Contains(response.Body.String(), "SELFHOST_HTTP_PASSWORD_CANARY") {
		t.Fatal("selfhost failure exposed a database credential")
	}
}

func TestSelfhostMixedOperationalFailureDoesNotBecomeDenial(t *testing.T) {
	request, logs, reference := diagnosticRequest("/v1/network/selfhost/installations")
	response := httptest.NewRecorder()
	private := errors.New("SELFHOST_PRIVATE_SQL_DETAIL")
	selfhostError(response, request, errors.Join(selfhost.ErrUnauthorized, syscall.EIO, private))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("mixed denial and I/O failure became status %d", response.Code)
	}
	var body ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "selfhost_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
		t.Fatalf("response omitted safe failure identity: %+v", body.Error)
	}
	var event map[string]any
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("decode single diagnostic event: %v (%s)", err, logs.String())
	}
	if event["errno"] != float64(syscall.EIO) || event["code"] != "selfhost_unavailable" || event["support_reference"] != reference || event["correlation_id"] != "correlation_fault_01" {
		t.Fatalf("event omitted operational cause or identity: %s", logs.String())
	}
	if strings.Contains(logs.String(), private.Error()) || strings.Contains(response.Body.String(), private.Error()) {
		t.Fatal("selfhost failure exposed private SQL detail")
	}
}

func TestSelfhostPureDenialKeepsPublicRejection(t *testing.T) {
	request, _, _ := diagnosticRequest("/v1/network/selfhost/installations")
	response := httptest.NewRecorder()
	selfhostError(response, request, selfhost.ErrUnauthorized)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"installation_unauthorized"`) {
		t.Fatalf("pure selfhost denial changed public response: %d %s", response.Code, response.Body.String())
	}
}

func openSelfhostClosedPostgres(t *testing.T) *db.DB {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("reserve a closed PostgreSQL test port")
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal("close the reserved PostgreSQL test port")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://selfhost_test:SELFHOST_HTTP_PASSWORD_CANARY@" + address + "/selfhost_test?sslmode=disable&connect_timeout=1"})
	if err != nil {
		t.Fatalf("open lazy SQL store (error type %T)", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
