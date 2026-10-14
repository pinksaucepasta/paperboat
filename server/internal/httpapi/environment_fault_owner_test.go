package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

func TestEnvironmentMixedFailuresReachOneCorrelatedDiagnosticOwner(t *testing.T) {
	const private = "PRIVATE_ENV_STORAGE_CAUSE"
	for _, test := range []struct {
		name string
		run  func(http.ResponseWriter, *http.Request)
	}{
		{"typed conflict", func(w http.ResponseWriter, r *http.Request) {
			writeEnvironmentError(w, r, errors.Join(&environment.VersionConflictError{CurrentVersion: 7}, syscall.EIO, errors.New(private)))
		}},
		{"vault authorization", func(w http.ResponseWriter, r *http.Request) {
			writeVaultScopeResult(w, r, nil, errors.Join(environment.ErrKeyAuthorizationRequired, syscall.EIO, errors.New(private)))
		}},
		{"password vault missing", func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "account_test"}}))
			environmentPasswordVaultGet(&passwordVaultAPIStub{getErr: errors.Join(environment.ErrNotFound, syscall.EIO, errors.New(private))}).ServeHTTP(w, r)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, logs, reference := diagnosticRequest("/v1/environment")
			response := httptest.NewRecorder()
			test.run(response, request)
			if response.Code != http.StatusInternalServerError || strings.Count(logs.String(), `"stage":"http_handler"`) != 1 {
				t.Fatal("mixed ENV fault did not reach exactly one HTTP diagnostic owner")
			}
			for _, expected := range []string{reference, "correlation_fault_01", `"errno":5`, `"http_status":500`} {
				if !strings.Contains(logs.String(), expected) {
					t.Fatal("HTTP diagnostic owner lost the original correlated IO evidence")
				}
			}
			if !strings.Contains(response.Body.String(), reference) || strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
				t.Fatal("response lost its support reference or exposed private cause text")
			}
		})
	}
}
