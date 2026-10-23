package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func TestEnrollmentHandlersPreserveOperationalDatabaseFailures(t *testing.T) {
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
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := controlplane.NewEnrollmentService(store, signer, nil, "https://control.example", "test-enrollment-key")
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	credential, err := signer.SignCredential(mint.CredentialInput{
		Issuer: "https://control.example", Audience: "paperboat-enrollment", Subject: "environment_fault_01",
		JTI: "enrollment-jti-fault-01", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		CredentialClass: "helper_enrollment", Scopes: []string{"helper:enroll"},
		EnvironmentID: "environment_fault_01", EnrollmentID: "enrollment_fault_01",
	})
	if err != nil {
		t.Fatal(err)
	}
	exchangeBody, err := json.Marshal(struct {
		Credential string `json:"credential"`
		PublicKey  string `json:"public_key"`
	}{credential, base64.RawURLEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		path    string
		body    string
		handler func() http.Handler
		setup   func(*http.Request)
	}{
		{
			name:    "issue helper enrollment",
			path:    "/v1/environments/environment_fault_01/helpers/enrollments",
			handler: func() http.Handler { return helperEnrollmentIssue(service) },
			setup: func(request *http.Request) {
				request.SetPathValue("environment_id", "environment_fault_01")
				request.Header.Set("Idempotency-Key", "enrollment-operation-fault-01")
			},
		},
		{
			name:    "replace helper",
			path:    "/v1/environments/environment_fault_01/helpers/helper_fault_01/replacement",
			body:    `{"edge_pool":"default"}`,
			handler: func() http.Handler { return helperReplacement(service) },
			setup: func(request *http.Request) {
				request.SetPathValue("environment_id", "environment_fault_01")
				request.SetPathValue("helper_id", "helper_fault_01")
				request.Header.Set("Idempotency-Key", "replacement-operation-fault-01")
			},
		},
		{
			name:    "exchange helper enrollment",
			path:    "/v1/helper-enrollments",
			body:    string(exchangeBody),
			handler: func() http.Handler { return helperEnrollmentExchange(service) },
			setup:   func(*http.Request) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest(tc.path)
			request.Method = http.MethodPost
			if tc.body != "" {
				request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(request.Context())
			}
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{
				User: auth.User{ID: "account_fault_01"},
			}))
			tc.setup(request)
			response := httptest.NewRecorder()
			tc.handler().ServeHTTP(response, request)

			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "authorization_unavailable" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_fault_01" {
				t.Fatalf("error response omitted safe failure identity: %+v", body.Error)
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("decode structured failure log: %v (%s)", err, logs.String())
			}
			for key, want := range map[string]string{
				"component":         "paperboat-server",
				"stage":             "http_handler",
				"code":              "authorization_unavailable",
				"support_reference": reference,
				"correlation_id":    "correlation_fault_01",
				"outcome":           "failed",
			} {
				if event[key] != want {
					t.Errorf("log %s=%v, want %q; log=%s", key, event[key], want, logs.String())
				}
			}
			if event["error_type"] == "" || event["cause"] == "" {
				t.Errorf("log omitted bounded cause metadata: %s", logs.String())
			}
		})
	}
}
