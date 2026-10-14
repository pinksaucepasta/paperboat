package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/inspectaccess"
)

func TestInspectorAccessOperationalDatabaseFailureStaysUnavailable(t *testing.T) {
	store, err := db.Open(config.Database{
		Driver: "pgx",
		DSN:    "postgres://paperboat:paperboat@127.0.0.1:1/paperboat_test?sslmode=disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	const privateToken = "INSPECTOR_PRIVATE_TOKEN_CANARY"
	requestBody, err := json.Marshal(inspectaccess.AccessRequest{
		Token:            privateToken,
		ResourceKind:     "preview",
		ResourceID:       "preview_private_canary",
		RouteID:          "preview_private_canary",
		Action:           "inspect",
		EdgeNodeID:       "edge_private_canary",
		EdgeProcessEpoch: "epoch_private_canary",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/inspector/access", strings.NewReader(string(requestBody)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Paperboat-Edge-Node-ID", "edge_private_canary")
	request.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_private_canary")
	response := httptest.NewRecorder()
	(&EdgeService{store: store}).handleInspectorAccess(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed-store inspector access status=%d, want 503: %s", response.Code, response.Body.String())
	}
	for _, private := range []string{privateToken, "preview_private_canary", "edge_private_canary"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("private request value %q reached response: %s", private, response.Body.String())
		}
	}
	var body struct {
		Code         string `json:"code"`
		Retryable    bool   `json:"retryable"`
		RetryAfterMS int    `json:"retryAfterMs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode unavailable response: %v: %s", err, response.Body.String())
	}
	if body.Code != "inspector_unavailable" || !body.Retryable || body.RetryAfterMS != 1000 || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("operational failure lost actionable retry contract: body=%+v Retry-After=%q", body, response.Header().Get("Retry-After"))
	}
}

func TestInspectorAccessRejectionRequiresDirectExpectedSentinel(t *testing.T) {
	if !inspectaccess.IsExactExpectedRejection(inspectaccess.ErrNotAuthorized) || !inspectaccess.IsExactExpectedRejection(inspectaccess.ErrInvalid) {
		t.Fatal("pure inspector denial was not expected")
	}
	if inspectaccess.IsExactExpectedRejection(errors.Join(inspectaccess.ErrNotAuthorized, syscall.EIO)) {
		t.Fatal("joined operational failure was classified as an expected denial")
	}
}
