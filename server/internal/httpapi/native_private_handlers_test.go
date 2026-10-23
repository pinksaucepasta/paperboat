package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type nativePrivateResolverFunc func(context.Context, nativeprivateaccess.Request) (nativeprivateaccess.Target, error)

func (f nativePrivateResolverFunc) ResolveNativePrivate(ctx context.Context, request nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
	return f(ctx, request)
}

func TestNativePrivateGrantKeepsOperationalCauseAndExpectedDenialStatus(t *testing.T) {
	requestBody := `{"operation_id":"operation_10000000-0000-4000-8000-000000000023","resource_kind":"machine_service","resource_id":"machine_10000000-0000-4000-8000-000000000024","route_id":"tcp:3000","protocol":"tcp"}`
	principalContext := func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "user_10000000-0000-4000-8000-000000000021"}, Client: &auth.ClientPrincipal{SessionID: "session_10000000-0000-4000-8000-000000000022"}}))
	}
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-database-detail"
	operational := errors.Join(nativeprivateaccess.ErrDenied, fmt.Errorf("query detail %s: %w", secret, syscall.ECONNREFUSED))
	service := &nativeprivateaccess.Service{Signer: signer, Issuer: "https://api.example.test", Resolver: nativePrivateResolverFunc(func(context.Context, nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
		return nativeprivateaccess.Target{}, operational
	})}
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelReports := context.WithCancel(context.Background())
	reportsDone := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(reportsDone) }()
	defer func() {
		cancelReports()
		<-reportsDone
	}()
	var logs strings.Builder
	handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(&logs, nil)), nativePrivateGrant(service))
	req := principalContext(httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(requestBody)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"native_private_unavailable"`) {
		t.Fatalf("operational resolver error = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatal("API response exposed private resolver detail")
	}
	if !strings.Contains(logs.String(), "connection_refused") || strings.Contains(logs.String(), secret) {
		t.Fatalf("safe operational log missing or private detail exposed: %s", logs.String())
	}
	service.Resolver = nativePrivateResolverFunc(func(context.Context, nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
		return nativeprivateaccess.Target{}, nativeprivateaccess.ErrDenied
	})
	deniedResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedResponse, principalContext(httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(requestBody))))
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("expected resource denial = %d %s", deniedResponse.Code, deniedResponse.Body.String())
	}

	cancelReports()
	<-reportsDone
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 {
		t.Fatalf("support records = %d; want one unexpected-failure record", len(store.records))
	}
	record := store.records[0]
	if record.Cause != "connection_refused" || record.Errno != int(syscall.ECONNREFUSED) || !strings.Contains(response.Body.String(), record.Reference) {
		t.Fatalf("support record lost original cause or request correlation: %+v", record)
	}
}

func TestNativePrivateGrantUsesAuthenticatedMachineIdentity(t *testing.T) {
	now := time.Now().UTC()
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := &nativeprivateaccess.Service{Signer: signer, Issuer: "https://api.example.test", Now: func() time.Time { return now }}
	service.Resolver = nativePrivateResolverFunc(func(_ context.Context, request nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
		if request.AccountID != "user_10000000-0000-4000-8000-000000000021" || request.UserID != "user_10000000-0000-4000-8000-000000000021" || request.CLIClientSessionID != "session_10000000-0000-4000-8000-000000000022" || request.OperationID != "operation_10000000-0000-4000-8000-000000000023" {
			t.Fatalf("request=%+v", request)
		}
		return nativeprivateaccess.Target{AccountID: request.AccountID, UserID: request.UserID, EnvironmentID: "machine_10000000-0000-4000-8000-000000000024", MachineID: "machine_10000000-0000-4000-8000-000000000024", AccessSessionID: "session_10000000-0000-4000-8000-000000000025", ResourceKind: "machine_service", ResourceID: "machine_10000000-0000-4000-8000-000000000024", ResourceGeneration: 2, RouteID: "tcp:3000", RouteGeneration: 2, TargetGeneration: 2, Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:3000", InstallationGeneration: 2, PolicyGeneration: 2, AnnouncementGeneration: 2, BootID: "boot_10000000-0000-4000-8000-000000000026"}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(`{"operation_id":"operation_10000000-0000-4000-8000-000000000023","resource_kind":"machine_service","resource_id":"machine_10000000-0000-4000-8000-000000000024","route_id":"tcp:3000","protocol":"tcp"}`))
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "user_10000000-0000-4000-8000-000000000021"}, Client: &auth.ClientPrincipal{SessionID: "session_10000000-0000-4000-8000-000000000022"}}))
	recorder := httptest.NewRecorder()
	nativePrivateGrant(service).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), `"credential"`) {
		t.Fatalf("status=%d cache=%q body=%s", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Body.String())
	}
}

func TestNativePrivateGrantRequiresMachinePrincipalAndStrictBody(t *testing.T) {
	signer, _ := mint.NewEphemeral(time.Hour)
	service := &nativeprivateaccess.Service{Signer: signer, Issuer: "https://api.example.test", Resolver: nativePrivateResolverFunc(func(context.Context, nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
		return nativeprivateaccess.Target{}, nativeprivateaccess.ErrDenied
	})}
	for name, testCase := range map[string]struct {
		request *http.Request
		want    int
	}{
		"no machine principal": {httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(`{}`)), http.StatusUnauthorized},
		"unknown field":        {httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(`{"operation_id":"operation_10000000-0000-4000-8000-000000000023","unknown":true}`)).WithContext(context.WithValue(context.Background(), authContextKey{}, principal{User: auth.User{ID: "user_10000000-0000-4000-8000-000000000021"}, Client: &auth.ClientPrincipal{SessionID: "session_10000000-0000-4000-8000-000000000022"}})), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			nativePrivateGrant(service).ServeHTTP(recorder, testCase.request)
			if recorder.Code != testCase.want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}
}

// A port the policy allows but the machine has not announced yet (a service started moments ago) is a
// retryable 409, not the final 403 of a policy denial (Lane A, access-machine 0-byte failures).
func TestNativePrivateGrantNotYetAnnouncedPortIsRetryable(t *testing.T) {
	requestBody := `{"operation_id":"operation_10000000-0000-4000-8000-000000000031","resource_kind":"machine_service","resource_id":"machine_10000000-0000-4000-8000-000000000032","route_id":"tcp:3000","protocol":"tcp"}`
	signer, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		resolverErr error
		status      int
		code        string
		retryAfter  string
	}{
		"not announced yet": {nativeprivateaccess.ErrServiceNotAnnounced, http.StatusConflict, "service_not_announced", "2"},
		"policy denial":     {nativeprivateaccess.ErrDenied, http.StatusForbidden, "forbidden", ""},
	} {
		t.Run(name, func(t *testing.T) {
			service := &nativeprivateaccess.Service{Signer: signer, Issuer: "https://api.example.test", Resolver: nativePrivateResolverFunc(func(context.Context, nativeprivateaccess.Request) (nativeprivateaccess.Target, error) {
				return nativeprivateaccess.Target{}, test.resolverErr
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/native-private-access/grants", strings.NewReader(requestBody))
			req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, principal{User: auth.User{ID: "user_10000000-0000-4000-8000-000000000033"}, Client: &auth.ClientPrincipal{SessionID: "session_10000000-0000-4000-8000-000000000034"}}))
			response := httptest.NewRecorder()
			nativePrivateGrant(service).ServeHTTP(response, req)
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || response.Header().Get("Retry-After") != test.retryAfter {
				t.Fatalf("%s = %d %q Retry-After=%q", name, response.Code, response.Body.String(), response.Header().Get("Retry-After"))
			}
		})
	}
}
