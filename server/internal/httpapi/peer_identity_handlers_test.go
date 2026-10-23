package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type recordingCertificateRegistrar struct {
	request peeridentity.RegisterRequest
	result  wire.Certificate
	err     error
}

type recordingBootstrapper struct {
	request peeridentity.BootstrapRequest
	result  wire.Certificate
	err     error
}

func (b *recordingBootstrapper) Bootstrap(_ context.Context, request peeridentity.BootstrapRequest) (wire.Certificate, error) {
	b.request = request
	return b.result, b.err
}

type failingBootstrapRootReadback struct {
	*recordingBootstrapper
	err error
}

func (b failingBootstrapRootReadback) Root(context.Context, string) (peeridentity.AccountRoot, error) {
	return peeridentity.AccountRoot{}, b.err
}

type e2eeRootReaderFunc func(context.Context, string) (peeridentity.AccountRoot, error)

func (f e2eeRootReaderFunc) Root(ctx context.Context, userID string) (peeridentity.AccountRoot, error) {
	return f(ctx, userID)
}

func TestE2EEBootstrapBindsAuthenticatedCLISession(t *testing.T) {
	rootPublic := bytes.Repeat([]byte{3}, 32)
	raw := bytes.Repeat([]byte{4}, 172)
	rootFingerprint := sha256.Sum256(rootPublic)
	certificateFingerprint := sha256.Sum256(raw)
	issued := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	expires := issued.Add(time.Hour)
	keyID, _ := peeridentity.KeyID(rootPublic)
	document := wire.BootstrapRequestDocument{RootPublicKey: base64.RawURLEncoding.EncodeToString(rootPublic), Certificate: wire.CertificateDocument{Version: 1, AccountID: "account_01", KeyID: keyID, EndpointID: "cli_session_01", Role: "cli", Generation: 1, Serial: 1, IssuedAt: issued.Format(time.RFC3339), ExpiresAt: expires.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(certificateFingerprint[:])}}
	body, _ := json.Marshal(document)
	bootstrapper := &recordingBootstrapper{result: wire.Certificate{AccountID: "account_01", KeyID: keyID, Role: wire.RoleCLI, EndpointID: "cli_session_01", Generation: 1, Serial: 1, IssuedAt: issued, ExpiresAt: expires, RootFingerprint: rootFingerprint, Fingerprint: certificateFingerprint, Raw: raw}}
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/bootstrap", bytes.NewReader(body))
	request.Header.Set("Idempotency-Key", "operation_bootstrap_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	e2eeBootstrap(bootstrapper).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || bootstrapper.request.CLIClientSessionID != "cli_session_01" || bootstrapper.request.Expected.EndpointID != "cli_session_01" || !bytes.Equal(bootstrapper.request.RootPublicKey, rootPublic) {
		t.Fatalf("status=%d request=%+v body=%s", response.Code, bootstrapper.request, response.Body.String())
	}
}

func TestE2EERootOperationalFailureKeepsCauseAtHTTPBoundary(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	const (
		reference        = "support_12345678-1234-4234-8234-123456789abc"
		correlationValue = "correlation_e2ee_root_01"
		requestIDValue   = "request_e2ee_root_01"
		privateCause     = "E2EE_ROOT_STORAGE_SECRET"
	)
	request := httptest.NewRequest(http.MethodGet, "/v1/e2ee/root", nil)
	request.Header.Set("Support-Reference", reference)
	request.Header.Set("Correlation-Id", correlationValue)
	request.Header.Set("Request-Id", requestIDValue)
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reports.SetLogger(logger)
	rootErr := errors.Join(syscall.ECONNREFUSED, errors.New(privateCause))
	reader := e2eeRootReaderFunc(func(context.Context, string) (peeridentity.AccountRoot, error) {
		return peeridentity.AccountRoot{}, rootErr
	})
	handler := requestID(correlationID(supportMiddleware(reports, logger, e2eeRootGet(reader))))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), privateCause) || strings.Contains(logs.String(), privateCause) {
		t.Fatalf("status=%d response=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) != 0 {
			if len(records) != 1 {
				t.Fatalf("captured %d root read failures, want one", len(records))
			}
			record := records[0]
			if record.Cause != "connection_refused" || record.Errno != int(syscall.ECONNREFUSED) || record.HTTPStatus != http.StatusServiceUnavailable || record.Code != "temporarily_unavailable" || record.Reference != reference || record.CorrelationID != correlationValue || record.RequestID != requestIDValue {
				t.Fatalf("HTTP boundary lost root-read cause or request identity: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("E2EE root operational failure was not captured")
}

func TestE2EEBootstrapReadbackFailureIsObservedAfterCommit(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	const (
		reference        = "support_12345678-1234-4234-8234-123456789abc"
		correlationValue = "correlation_bootstrap_readback_01"
		requestIDValue   = "request_bootstrap_readback_01"
		privateCause     = "ROOT_READBACK_SECRET"
	)
	rootPublic := bytes.Repeat([]byte{3}, 32)
	raw := bytes.Repeat([]byte{4}, 172)
	rootFingerprint := sha256.Sum256(rootPublic)
	certificateFingerprint := sha256.Sum256(raw)
	issued := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	expires := issued.Add(time.Hour)
	keyID, _ := peeridentity.KeyID(rootPublic)
	document := wire.BootstrapRequestDocument{RootPublicKey: base64.RawURLEncoding.EncodeToString(rootPublic), Certificate: wire.CertificateDocument{Version: 1, AccountID: "account_01", KeyID: keyID, EndpointID: "cli_session_01", Role: "cli", Generation: 1, Serial: 1, IssuedAt: issued.Format(time.RFC3339), ExpiresAt: expires.Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(certificateFingerprint[:])}}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapper := &recordingBootstrapper{result: wire.Certificate{AccountID: "account_01", KeyID: keyID, Role: wire.RoleCLI, EndpointID: "cli_session_01", Generation: 1, Serial: 1, IssuedAt: issued, ExpiresAt: expires, RootFingerprint: rootFingerprint, Fingerprint: certificateFingerprint, Raw: raw}}
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/bootstrap", bytes.NewReader(body))
	request.Header.Set("Idempotency-Key", "operation_bootstrap_01")
	request.Header.Set("Support-Reference", reference)
	request.Header.Set("Correlation-Id", correlationValue)
	request.Header.Set("Request-Id", requestIDValue)
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reports.SetLogger(logger)
	mixedReadbackFailure := errors.Join(syscall.EIO, errors.New(privateCause))
	service := failingBootstrapRootReadback{recordingBootstrapper: bootstrapper, err: mixedReadbackFailure}
	handler := requestID(correlationID(supportMiddleware(reports, logger, e2eeBootstrap(service))))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), privateCause) || strings.Contains(logs.String(), privateCause) {
		t.Fatalf("status=%d response=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
	var envelope struct {
		Data wire.BootstrapDocument `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.KeyID != keyID || len(envelope.Data.TrustedKeys) != 1 || envelope.Data.TrustedKeys[0].KeyID != keyID {
		t.Fatalf("committed bootstrap fallback identity missing: data=%+v err=%v", envelope.Data, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) != 0 {
			if len(records) != 1 {
				t.Fatalf("captured %d readback failures, want one observation", len(records))
			}
			record := records[0]
			if record.Outcome != "failed" || record.Severity != "warning" || record.Stage != "service_error" || record.Code != "temporarily_unavailable" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.Reference != reference || record.CorrelationID != correlationValue || record.RequestID != requestIDValue || record.OwnerID != "account_01" {
				t.Fatalf("readback observation lost committed request identity or cause: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("committed bootstrap readback failure was not observed")
}

func TestE2EEBootstrapReportsSecretSafeValidationStage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/bootstrap", bytes.NewBufferString(`{"root_public_key":"not-base64","certificate":{"version":1,"account_id":"account_01","endpoint_id":"cli_session_01","role":"cli","generation":1,"serial":1}}`))
	request.Header.Set("Idempotency-Key", "operation_bootstrap_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	e2eeBootstrap(&recordingBootstrapper{}).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"stage":"root_public_key_encoding"`)) || bytes.Contains(response.Body.Bytes(), []byte("not-base64")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestE2EEBootstrapReportsSafeServiceErrorClass(t *testing.T) {
	rootPublic := bytes.Repeat([]byte{3}, 32)
	raw := bytes.Repeat([]byte{4}, 172)
	certificateFingerprint := sha256.Sum256(raw)
	issued := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	keyID, _ := peeridentity.KeyID(rootPublic)
	document := wire.BootstrapRequestDocument{RootPublicKey: base64.RawURLEncoding.EncodeToString(rootPublic), Certificate: wire.CertificateDocument{Version: 1, AccountID: "account_01", KeyID: keyID, EndpointID: "cli_session_01", Role: "cli", Generation: 1, Serial: 1, IssuedAt: issued.Format(time.RFC3339), ExpiresAt: issued.Add(time.Hour).Format(time.RFC3339), Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(certificateFingerprint[:])}}
	body, _ := json.Marshal(document)
	request := httptest.NewRequest(http.MethodPost, "/v1/e2ee/bootstrap", bytes.NewReader(body))
	request.Header.Set("Idempotency-Key", "operation_bootstrap_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}, Client: &auth.ClientPrincipal{SessionID: "cli_session_01"}}))
	response := httptest.NewRecorder()
	e2eeBootstrap(&recordingBootstrapper{err: wire.ErrInvalid}).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"stage":"service"`)) || !bytes.Contains(response.Body.Bytes(), []byte(`"reason":"invalid"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func (r *recordingCertificateRegistrar) Register(_ context.Context, request peeridentity.RegisterRequest) (wire.Certificate, error) {
	r.request = request
	return r.result, r.err
}

func TestEndpointCertificateRegisterMapsCanonicalDocument(t *testing.T) {
	raw := bytes.Repeat([]byte{1}, 172)
	rootFingerprint := sha256.Sum256([]byte("root"))
	keyID := "aek_" + hex.EncodeToString(rootFingerprint[:])
	certificateFingerprint := sha256.Sum256(raw)
	issued := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	expires := issued.Add(time.Hour)
	registrar := &recordingCertificateRegistrar{result: wire.Certificate{
		AccountID: "account_01", KeyID: keyID, Role: wire.RoleCLI, EndpointID: "cli_01", Generation: 2, Serial: 7,
		IssuedAt: issued, ExpiresAt: expires, RootFingerprint: rootFingerprint, Fingerprint: certificateFingerprint, Raw: raw,
	}}
	document := wire.CertificateDocument{
		Version: 1, AccountID: "account_01", KeyID: keyID, EndpointID: "cli_01",
		Role: "cli", Generation: 2, Serial: 7, IssuedAt: issued.Format(time.RFC3339), ExpiresAt: expires.Format(time.RFC3339),
		Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(certificateFingerprint[:]),
	}
	body, _ := json.Marshal(document)
	request := httptest.NewRequest(http.MethodPut, "/v1/endpoints/cli_01/certificates/2", bytes.NewReader(body))
	request.SetPathValue("endpoint_id", "cli_01")
	request.SetPathValue("generation", "2")
	request.Header.Set("Idempotency-Key", "operation_endpoint_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}}))
	response := httptest.NewRecorder()
	endpointCertificateRegister(registrar).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || registrar.request.OperationID != "operation_endpoint_01" || registrar.request.Expected.EndpointID != "cli_01" || registrar.request.ExpectedCertificateFingerprint != certificateFingerprint {
		t.Fatalf("status=%d request=%+v body=%s", response.Code, registrar.request, response.Body.String())
	}
}

func TestEndpointCertificateRegisterRejectsNonCanonicalAndMapsConflicts(t *testing.T) {
	registrar := &recordingCertificateRegistrar{}
	request := httptest.NewRequest(http.MethodPut, "/v1/endpoints/cli_01/certificates/1", bytes.NewBufferString(`{"version":1}`))
	request.SetPathValue("endpoint_id", "cli_01")
	request.SetPathValue("generation", "1")
	request.Header.Set("Idempotency-Key", "operation_endpoint_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}}))
	response := httptest.NewRecorder()
	endpointCertificateRegister(registrar).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"invalid_request"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	raw := bytes.Repeat([]byte{1}, 172)
	fingerprint := sha256.Sum256(raw)
	document := wire.CertificateDocument{Version: 1, AccountID: "account_01", KeyID: "aek_" + hex.EncodeToString(fingerprint[:]), EndpointID: "cli_01", Role: "cli", Generation: 1, Serial: 1, IssuedAt: "2026-08-03T12:00:00Z", ExpiresAt: "2026-08-03T13:00:00Z", Certificate: base64.RawURLEncoding.EncodeToString(raw), CertificateFingerprint: hex.EncodeToString(fingerprint[:])}
	body, _ := json.Marshal(document)
	registrar.err = peeridentity.ErrConflict
	request = httptest.NewRequest(http.MethodPut, "/v1/endpoints/cli_01/certificates/1", bytes.NewReader(body))
	request.SetPathValue("endpoint_id", "cli_01")
	request.SetPathValue("generation", "1")
	request.Header.Set("Idempotency-Key", "operation_endpoint_01")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_01"}}))
	response = httptest.NewRecorder()
	endpointCertificateRegister(registrar).ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"operation_conflict"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	registrar.err = errors.New("unexpected")
}
