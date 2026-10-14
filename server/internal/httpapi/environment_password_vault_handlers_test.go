package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type passwordVaultAPIStub struct {
	getAccount string
	getErr     error
	putAccount string
	putRaw     []byte
	putErr     error
}

func (s *passwordVaultAPIStub) PasswordVaultIssuer() string {
	return "https://control.example"
}

func (s *passwordVaultAPIStub) GetPasswordVault(_ context.Context, account string) (environment.PasswordVaultHead, error) {
	s.getAccount = account
	if s.getErr != nil {
		return environment.PasswordVaultHead{}, s.getErr
	}
	return environment.PasswordVaultHead{Issuer: "https://control.example", AccountID: account, Generation: 1, DocumentID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Envelope: "AQ"}, nil
}
func (s *passwordVaultAPIStub) PutPasswordVault(_ context.Context, account string, raw []byte) (environment.PasswordVaultHead, error) {
	s.putAccount, s.putRaw = account, append([]byte(nil), raw...)
	if s.putErr != nil {
		return environment.PasswordVaultHead{}, s.putErr
	}
	return environment.PasswordVaultHead{Issuer: "https://control.example", AccountID: account, Generation: 1, DocumentID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Envelope: "AQ"}, nil
}

func TestEnvironmentPasswordVaultPutReportsCASConflict(t *testing.T) {
	stub := &passwordVaultAPIStub{putErr: environment.ErrPasswordVaultConflict}
	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_1"}})
	result := httptest.NewRecorder()
	environmentPasswordVaultPut(stub).ServeHTTP(result, httptest.NewRequest(http.MethodPut, "/v1/environment-vault", jsonReader(t, map[string]any{"envelope": "AQ"})).WithContext(ctx))
	if result.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", result.Code, result.Body.String())
	}
}

func TestEnvironmentPasswordVaultGetReturnsConfiguredIssuerBeforeGenesis(t *testing.T) {
	stub := &passwordVaultAPIStub{getErr: environment.ErrNotFound}
	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_1"}})
	result := httptest.NewRecorder()
	environmentPasswordVaultGet(stub).ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/v1/environment-vault", nil).WithContext(ctx))
	if result.Code != http.StatusNotFound || !bytes.Contains(result.Body.Bytes(), []byte(`"code":"not_found"`)) ||
		!bytes.Contains(result.Body.Bytes(), []byte(`"issuer":"https://control.example"`)) {
		t.Fatalf("genesis response status=%d body=%s", result.Code, result.Body.String())
	}
}

func TestEnvironmentPasswordVaultGetKeepsOperationalCauseJoinedWithNotFound(t *testing.T) {
	const privateCause = "storage-detail-must-not-reach-response"
	stub := &passwordVaultAPIStub{getErr: errors.Join(environment.ErrNotFound, errors.New(privateCause))}
	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_1"}})
	result := httptest.NewRecorder()
	environmentPasswordVaultGet(stub).ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/v1/environment-vault", nil).WithContext(ctx))
	if result.Code != http.StatusInternalServerError || bytes.Contains(result.Body.Bytes(), []byte(`"code":"not_found"`)) || bytes.Contains(result.Body.Bytes(), []byte(privateCause)) {
		t.Fatalf("joined storage failure status=%d body=%s", result.Code, result.Body.String())
	}
}

func TestWriteEnvironmentErrorMapsOnlyPureExpectedFailures(t *testing.T) {
	const privateCause = "storage-detail-must-not-reach-response"
	operational := errors.New(privateCause)
	tests := []struct {
		name string
		err  error
	}{
		{name: "validation", err: environment.ErrProtocolInvalid},
		{name: "precondition", err: environment.ErrPrecondition},
		{name: "not found", err: environment.ErrNotFound},
		{name: "machine not found", err: environment.ErrMachineNotFound},
		{name: "machine capability", err: environment.ErrMachineEnvironmentUnavailable},
		{name: "version conflict", err: &environment.VersionConflictError{CurrentVersion: 7}},
		{name: "vault conflict", err: environment.ErrPasswordVaultConflict},
		{name: "unconfigured", err: &environment.EnvironmentUnconfiguredError{MachineID: "coordinate-marker", HostPublic: "public-key-marker"}},
		{name: "authority conflict", err: environment.ErrAuthorityConflict},
		{name: "typed authority conflict", err: &environment.AuthorityConflictError{CurrentID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		{name: "transition in progress", err: environment.ErrTransitionInProgress},
		{name: "operation conflict", err: environment.ErrOperationConflict},
		{name: "key authorization", err: environment.ErrKeyAuthorizationRequired},
		{name: "authority fork", err: environment.ErrAuthorityFork},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := httptest.NewRecorder()
			writeEnvironmentError(result, httptest.NewRequest(http.MethodPost, "/v1/environment", nil), errors.Join(test.err, operational))
			if result.Code != http.StatusInternalServerError || bytes.Contains(result.Body.Bytes(), []byte(privateCause)) || bytes.Contains(result.Body.Bytes(), []byte("coordinate-marker")) || bytes.Contains(result.Body.Bytes(), []byte("public-key-marker")) {
				t.Fatalf("mixed failure status=%d body=%s", result.Code, result.Body.String())
			}
		})
	}
}

func TestWriteEnvironmentErrorKeepsPureTypedConflictStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "version", err: &environment.VersionConflictError{CurrentVersion: 7}, code: "version_conflict"},
		{name: "authority", err: &environment.AuthorityConflictError{CurrentID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, code: "authority_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := httptest.NewRecorder()
			writeEnvironmentError(result, httptest.NewRequest(http.MethodPost, "/v1/environment", nil), test.err)
			if result.Code != http.StatusConflict || !bytes.Contains(result.Body.Bytes(), []byte(`"code":"`+test.code+`"`)) {
				t.Fatalf("typed conflict status=%d body=%s", result.Code, result.Body.String())
			}
		})
	}
}

func TestEnvironmentPasswordVaultHandlersUseAuthenticatedAccount(t *testing.T) {
	stub := &passwordVaultAPIStub{}
	principalContext := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_authenticated"}})

	get := httptest.NewRequest(http.MethodGet, "/v1/environment-vault", nil).WithContext(principalContext)
	getResult := httptest.NewRecorder()
	environmentPasswordVaultGet(stub).ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK || stub.getAccount != "account_authenticated" || getResult.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status=%d account=%q headers=%v", getResult.Code, stub.getAccount, getResult.Header())
	}

	put := httptest.NewRequest(http.MethodPut, "/v1/environment-vault", jsonReader(t, map[string]any{"envelope": "AQ"})).WithContext(principalContext)
	putResult := httptest.NewRecorder()
	environmentPasswordVaultPut(stub).ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK || stub.putAccount != "account_authenticated" || len(stub.putRaw) != 1 || stub.putRaw[0] != 1 {
		t.Fatalf("PUT status=%d account=%q raw=%x", putResult.Code, stub.putAccount, stub.putRaw)
	}
}

func TestEnvironmentPasswordVaultHandlersRequirePrincipalAndStrictBody(t *testing.T) {
	stub := &passwordVaultAPIStub{}
	unauthenticated := httptest.NewRecorder()
	environmentPasswordVaultGet(stub).ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/v1/environment-vault", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status=%d", unauthenticated.Code)
	}

	ctx := context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "account_1"}})
	bad := httptest.NewRecorder()
	environmentPasswordVaultPut(stub).ServeHTTP(bad, httptest.NewRequest(http.MethodPut, "/v1/environment-vault", jsonReader(t, map[string]any{"envelope": "AQ", "account_id": "account_other"})).WithContext(ctx))
	if bad.Code != http.StatusBadRequest || stub.putAccount != "" {
		t.Fatalf("client-selected account accepted: status=%d account=%q", bad.Code, stub.putAccount)
	}
}

func jsonReader(t *testing.T, value any) *bytes.Reader {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}
