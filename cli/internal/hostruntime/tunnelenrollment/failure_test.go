package tunnelenrollment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestEnrollmentOutcomeOnlyAllowsRepeatedPureCausesAndRejectsUnsafeTrees(t *testing.T) {
	if !enrollmentOutcomeOnly(safeEnrollmentFailure("safe rejection", ErrAuthentication, ErrAuthentication), ErrAuthentication) {
		t.Fatal("repeated shared authentication causes did not prove a pure rejection")
	}
	if enrollmentOutcomeOnly(safeEnrollmentFailure("mixed failure", ErrAuthentication, ErrUnavailable), ErrAuthentication) {
		t.Fatal("mixed operational cause proved a pure authentication rejection")
	}
	if enrollmentOutcomeOnly(safeEnrollmentClassFailure("unavailable", ErrUnavailable, ErrAuthentication), ErrAuthentication) {
		t.Fatal("unavailable-purpose wrapper was reclassified as authentication rejection")
	}
	if enrollmentOutcomeOnly(safeEnrollmentClassFailure("mixed rejection", ErrAuthentication, safeEnrollmentFailure("mixed", ErrAuthentication, ErrUnavailable)), ErrAuthentication) {
		t.Fatal("authentication purpose concealed a mixed operational cause")
	}

	cycle := &enrollmentOutcomeCycle{}
	cycle.next = cycle
	if enrollmentOutcomeOnly(cycle, ErrAuthentication) {
		t.Fatal("cyclic cause tree proved an authentication rejection")
	}
	var typedNil error = (*enrollmentOutcomeCycle)(nil)
	if enrollmentOutcomeOnly(typedNil, ErrAuthentication) {
		t.Fatal("typed-nil cause proved an authentication rejection")
	}
	tooMany := make(enrollmentOutcomeBranch, 17)
	for i := range tooMany {
		tooMany[i] = ErrAuthentication
	}
	if enrollmentOutcomeOnly(tooMany, ErrAuthentication) {
		t.Fatal("over-budget cause tree proved an authentication rejection")
	}
}

func TestCredentialStoreFailuresKeepTypedCauseWithoutPathText(t *testing.T) {
	t.Run("create directory", func(t *testing.T) {
		stateRoot := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(stateRoot, []byte("marker"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := NewFileCredentialStore(stateRoot)
		assertSafePathFailure(t, err, stateRoot, ErrSecretStore)
	})

	t.Run("read journal", func(t *testing.T) {
		store, err := NewFileCredentialStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		journalPath := filepath.Join(store.root, "journal.json")
		if err := os.Mkdir(journalPath, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err = store.loadJournal()
		assertSafePathFailure(t, err, journalPath, ErrSecretStore)
		if errors.Is(err, ErrConflict) {
			t.Fatalf("operational journal read was misclassified as conflict: %v", err)
		}
	})

	t.Run("save journal", func(t *testing.T) {
		store, err := NewFileCredentialStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root := store.root
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		err = store.saveJournal(journal{Version: 1, Records: map[string]record{}})
		assertSafePathFailure(t, err, root, ErrSecretStore)
	})
}

func TestJournalParserCauseIsRetainedBehindStaticError(t *testing.T) {
	store, err := NewFileCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := "private-journal-field"
	data := []byte(`{"` + marker + `":"private journal content","broken":?}`)
	if err := os.WriteFile(filepath.Join(store.root, "journal.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.loadJournal()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid journal error=%v, want conflict", err)
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), string(data)) {
		t.Fatalf("journal error exposed stored content: %v", err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("journal parse cause was lost: %T %v", err, err)
	}
}

func TestServerClientKeepsReadAndCloseCausesWithoutResponseText(t *testing.T) {
	readErr := errors.New("private response read details")
	closeErr := errors.New("private response close details")
	transport := responseTransport{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       failingResponseBody{readErr: readErr, closeErr: closeErr},
	}}
	client, err := newServerClient("https://control.example", &bootstrapMachineAuth{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.do(context.Background(), http.MethodPost, "/v1/example", "operation_01", []byte(`{}`), &struct{}{})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, readErr) || !errors.Is(err, closeErr) {
		t.Fatalf("response failure=%v, want unavailable plus original read/close causes", err)
	}
	assertStaticFailureText(t, err, "private response")
}

func TestServerClientTreatsProviderFailuresAsUnavailableUnlessExplicitAuthDenial(t *testing.T) {
	operational := errors.New("private credential storage details")
	for _, test := range []struct {
		name      string
		auth      failureMachineAuth
		wantAuth  bool
		wantCause error
	}{
		{name: "token storage failure", auth: failureMachineAuth{tokenErr: operational}, wantCause: operational},
		{name: "proof storage failure", auth: failureMachineAuth{proofErr: operational}, wantCause: operational},
		{name: "explicit auth denial", auth: failureMachineAuth{tokenErr: ErrAuthentication}, wantAuth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := newServerClient("https://control.example", test.auth, responseTransport{err: errors.New("unused transport")})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.do(context.Background(), http.MethodPost, "/v1/example", "operation_01", []byte(`{}`), &struct{}{})
			if test.wantAuth {
				if err != ErrAuthentication {
					t.Fatalf("auth provider denial=%v, want ErrAuthentication", err)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrAuthentication) || !errors.Is(err, test.wantCause) {
				t.Fatalf("auth provider operational failure=%v, want unavailable and original cause only", err)
			}
			assertStaticFailureText(t, err, "private credential storage")
		})
	}
}

func TestLocalClientKeepsTransportCauseWithoutTransportText(t *testing.T) {
	transportErr := errors.New("private local transport details")
	client, err := NewLocalClient("http://127.0.0.1:43821", "local-token", &http.Client{Transport: responseTransport{err: transportErr}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Enroll(context.Background(), "tunnel_01", "operation_01")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, transportErr) {
		t.Fatalf("local transport failure=%v, want unavailable plus original cause", err)
	}
	assertStaticFailureText(t, err, "private local transport")
}

func TestLocalHandlerRequiresPureConflictBeforeReturningConflictStatus(t *testing.T) {
	for _, test := range []struct {
		name            string
		failJournal     bool
		upstreamStatus  int
		wantStatus      int
		wantBodyCode    string
		wantLocalFaults int
	}{
		{name: "pure server conflict", upstreamStatus: http.StatusConflict, wantStatus: http.StatusConflict, wantBodyCode: "enrollment_conflict"},
		{name: "pure server authentication denial", upstreamStatus: http.StatusUnauthorized, wantStatus: http.StatusUnauthorized, wantBodyCode: "authentication_required"},
		{name: "pure server authorization denial", upstreamStatus: http.StatusForbidden, wantStatus: http.StatusForbidden, wantBodyCode: "forbidden"},
		{name: "conflict followed by journal failure", failJournal: true, upstreamStatus: http.StatusConflict, wantStatus: http.StatusServiceUnavailable, wantBodyCode: "credential_store_unavailable", wantLocalFaults: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewFileCredentialStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(ManagerConfig{
				ControlURL: "https://control.example", HostID: "host_01", Auth: &testAuth{},
				Transport:   journalStatusTransport{store: store, failJournal: test.failJournal, status: test.upstreamStatus},
				Credentials: store, Activator: &testActivator{}, ControlToken: "local-token",
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(requestDocument{Schema: Schema, Kind: "tunnel_connector_enrollment_request", TunnelID: "tunnel_01"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/tunnel-connectors/enroll", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer local-token")
			request.Header.Set("Idempotency-Key", "operation_01")
			localFaults := 0
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
				if fault.Component == "paperboat-daemon" && fault.Stage == "local_gateway" {
					localFaults++
				}
			})
			defer restore()
			recorder := httptest.NewRecorder()
			manager.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), `"code":"`+test.wantBodyCode+`"`) {
				t.Fatalf("local response status=%d body=%s, want status=%d code=%s", recorder.Code, recorder.Body.String(), test.wantStatus, test.wantBodyCode)
			}
			if localFaults != test.wantLocalFaults {
				t.Fatalf("local gateway observations=%d, want %d", localFaults, test.wantLocalFaults)
			}
		})
	}
}

func TestLocalHandlerMapsRepeatedRejectionButNotUnavailablePurpose(t *testing.T) {
	for _, test := range []struct {
		name         string
		tokenErr     error
		wantStatus   int
		wantBodyCode string
	}{
		{
			name:         "repeated pure rejection",
			tokenErr:     safeEnrollmentFailure("safe rejection", ErrAuthentication, ErrAuthentication),
			wantStatus:   http.StatusUnauthorized,
			wantBodyCode: "authentication_required",
		},
		{
			name:         "unavailable purpose around rejection cause",
			tokenErr:     safeEnrollmentClassFailure("credential service unavailable", ErrUnavailable, ErrAuthentication),
			wantStatus:   http.StatusServiceUnavailable,
			wantBodyCode: "runtime_unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewFileCredentialStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(ManagerConfig{
				ControlURL: "https://control.example", HostID: "host_01",
				Auth:        failureMachineAuth{tokenErr: test.tokenErr},
				Transport:   responseTransport{err: errors.New("unused transport")},
				Credentials: store, Activator: &testActivator{}, ControlToken: "local-token",
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(requestDocument{Schema: Schema, Kind: "tunnel_connector_enrollment_request", TunnelID: "tunnel_01"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/tunnel-connectors/enroll", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer local-token")
			request.Header.Set("Idempotency-Key", "operation_01")
			recorder := httptest.NewRecorder()
			manager.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), `"code":"`+test.wantBodyCode+`"`) {
				t.Fatalf("handler status=%d body=%s, want status=%d code=%s", recorder.Code, recorder.Body.String(), test.wantStatus, test.wantBodyCode)
			}
		})
	}
}

func TestLocalClientCorrelationAndDaemonObservationForMixedFailure(t *testing.T) {
	store, err := NewFileCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		ControlURL: "https://control.example", HostID: "host_01", Auth: &testAuth{},
		Transport:   journalStatusTransport{store: store, failJournal: true, status: http.StatusServiceUnavailable},
		Credentials: store, Activator: &testActivator{}, ControlToken: "local-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	var receipts localHTTPReceipts
	server := httptest.NewServer(manager)
	defer server.Close()
	recordingTransport := &localHTTPRecordingTransport{base: http.DefaultTransport, receipts: &receipts}
	local, err := NewLocalClient(server.URL, "local-token", &http.Client{Transport: recordingTransport})
	if err != nil {
		t.Fatal(err)
	}
	var faultsMu sync.Mutex
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faultsMu.Lock()
		faults = append(faults, fault)
		faultsMu.Unlock()
	})
	defer restore()

	reference := supportref.New()
	if !supportref.Valid(reference) {
		t.Fatal("support reference generation failed")
	}
	_, err = local.Enroll(supportref.WithContext(context.Background(), reference), "tunnel_01", "operation_01")
	if !errors.Is(err, ErrSecretStore) {
		t.Fatalf("mixed local enrollment error=%v, want safe credential-store result", err)
	}
	firstReceipt := receipts.at(0)
	assertLocalUnavailableReceipt(t, firstReceipt)
	if firstReceipt.reference != reference {
		t.Fatalf("LocalClient sent support reference %q, want %q", firstReceipt.reference, reference)
	}

	invalidReference := "support_not-a-valid-reference"
	body, err := json.Marshal(requestDocument{Schema: Schema, Kind: "tunnel_connector_enrollment_request", TunnelID: "tunnel_02"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/tunnel-connectors/enroll", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-token")
	request.Header.Set("Idempotency-Key", "operation_02")
	request.Header.Set(supportref.Header, invalidReference)
	response, err := (&http.Client{Transport: recordingTransport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	secondReceipt := receipts.at(1)
	assertLocalUnavailableReceipt(t, secondReceipt)
	if secondReceipt.reference != invalidReference {
		t.Fatalf("test request did not send malformed reference: %q", secondReceipt.reference)
	}

	faultsMu.Lock()
	observed := append([]errorreport.Fault(nil), faults...)
	faultsMu.Unlock()
	var localFaults []errorreport.Fault
	for _, fault := range observed {
		if fault.Component == "paperboat-daemon" && fault.Stage == "local_gateway" {
			localFaults = append(localFaults, fault)
		}
	}
	if len(localFaults) != 2 {
		t.Fatalf("local gateway observations=%d, want one per mixed request; all faults=%+v", len(localFaults), observed)
	}
	if localFaults[0].Operation != "tunnel_enrollment" || localFaults[0].Code != "local_gateway_failed" || localFaults[0].SupportReference != reference {
		t.Fatalf("first local gateway fault=%+v, want correlated enrollment failure", localFaults[0])
	}
	if !supportref.Valid(localFaults[1].SupportReference) || localFaults[1].SupportReference == invalidReference {
		t.Fatalf("invalid incoming reference became daemon metadata: %+v", localFaults[1])
	}
	for _, fault := range localFaults {
		if fault.SupportReference == "" || strings.Contains(fault.ErrorType, "private") || strings.Contains(strings.Join(fault.ErrorChain, ","), "private") {
			t.Fatalf("unsafe or uncorrelated daemon fault: %+v", fault)
		}
	}
}

func TestLocalHandlerSuppressesOnlyAnAlreadyObservedUnmixedAttempt(t *testing.T) {
	store, err := NewFileCredentialStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerConfig{
		ControlURL: "https://control.example", HostID: "host_01", Auth: &testAuth{},
		Transport:   journalStatusTransport{store: store, status: http.StatusServiceUnavailable},
		Credentials: store, Activator: &testActivator{}, ControlToken: "local-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(requestDocument{Schema: Schema, Kind: "tunnel_connector_enrollment_request", TunnelID: "tunnel_01"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tunnel-connectors/enroll", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer local-token")
	request.Header.Set("Idempotency-Key", "operation_01")
	var mu sync.Mutex
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, fault)
		mu.Unlock()
	})
	defer restore()
	recorder := httptest.NewRecorder()
	manager.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("local status=%d body=%q, want 503", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	observed := append([]errorreport.Fault(nil), faults...)
	mu.Unlock()
	localGateway, attempts := 0, 0
	for _, fault := range observed {
		if fault.Stage == "local_gateway" && fault.Component == "paperboat-daemon" {
			localGateway++
		}
		if fault.Stage == "control_request" {
			attempts++
		}
	}
	if attempts != 1 || localGateway != 0 {
		t.Fatalf("observed control attempts=%d local gateway failures=%d, want one attempt and no duplicate final observation: %+v", attempts, localGateway, observed)
	}
}

func assertLocalUnavailableReceipt(t *testing.T, receipt localHTTPReceipt) {
	t.Helper()
	if receipt.status != http.StatusServiceUnavailable {
		t.Fatalf("local HTTP status=%d body=%q, want 503", receipt.status, receipt.body)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(receipt.body), &envelope); err != nil || envelope.Error.Code != "credential_store_unavailable" {
		t.Fatalf("local HTTP error body=%q, decode=%v, want static credential_store_unavailable", receipt.body, err)
	}
	if strings.Contains(receipt.body, "control.example") || strings.Contains(receipt.body, "private") {
		t.Fatalf("local HTTP response exposed internal failure details: %q", receipt.body)
	}
}

type localHTTPReceipt struct {
	status    int
	body      string
	reference string
}

type localHTTPReceipts struct {
	mu     sync.Mutex
	values []localHTTPReceipt
}

func (receipts *localHTTPReceipts) append(receipt localHTTPReceipt) {
	receipts.mu.Lock()
	receipts.values = append(receipts.values, receipt)
	receipts.mu.Unlock()
}

func (receipts *localHTTPReceipts) at(index int) localHTTPReceipt {
	receipts.mu.Lock()
	defer receipts.mu.Unlock()
	if index >= len(receipts.values) {
		return localHTTPReceipt{}
	}
	return receipts.values[index]
}

type localHTTPRecordingTransport struct {
	base     http.RoundTripper
	receipts *localHTTPReceipts
}

func (transport *localHTTPRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	raw, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	if readErr != nil || closeErr != nil {
		return response, errors.Join(err, readErr, closeErr)
	}
	transport.receipts.append(localHTTPReceipt{status: response.StatusCode, body: string(raw), reference: request.Header.Get(supportref.Header)})
	return response, nil
}

func TestCarrierBootstrapKeepsBodyCausesWithoutResponseText(t *testing.T) {
	readErr := errors.New("private carrier response read details")
	closeErr := errors.New("private carrier response close details")
	endpoint, _ := url.Parse("https://control.example")
	source := &HTTPSProductionAssemblySource{
		base: endpoint,
		auth: &bootstrapMachineAuth{},
		http: &http.Client{Transport: responseTransport{response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       failingResponseBody{readErr: readErr, closeErr: closeErr},
		}}},
	}
	_, err := source.fetchCarrierDescriptor(context.Background(), ActivationRequest{TunnelID: "tunnel_01", ConnectorID: "connector_01"}, []byte(`{}`))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, readErr) || !errors.Is(err, closeErr) {
		t.Fatalf("carrier response failure=%v, want unavailable plus original read/close causes", err)
	}
	assertStaticFailureText(t, err, "private carrier response")
}

func TestControlHandshakeKeepsResponseCloseCauseWithoutResponseText(t *testing.T) {
	closeErr := errors.New("private handshake body close details")
	endpoint, _ := url.Parse("https://control.example")
	request, _ := productionActivationRequest(t)
	body := &closeFailureBody{reader: strings.NewReader("private handshake response"), closeErr: closeErr}
	source := &HTTPSProductionAssemblySource{
		base: endpoint,
		http: &http.Client{Transport: responseTransport{response: &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Header:     make(http.Header),
			Body:       body,
		}}},
	}
	_, err := source.openControlStream(context.Background(), request)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, closeErr) {
		t.Fatalf("control handshake failure=%v, close calls=%d, want unavailable plus response close cause", err, body.closed)
	}
	var diagnostic *ActivationDiagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Code != ActivationDiagnosticControlHTTPDenied {
		t.Fatalf("control handshake diagnostic=%+v, error=%v", diagnostic, err)
	}
	assertStaticFailureText(t, err, "private handshake")
}

type responseTransport struct {
	response *http.Response
	err      error
}

type enrollmentOutcomeCycle struct{ next error }

func (*enrollmentOutcomeCycle) Error() string { return "cyclic enrollment cause" }
func (cycle *enrollmentOutcomeCycle) Unwrap() error {
	if cycle == nil {
		return nil
	}
	return cycle.next
}

type enrollmentOutcomeBranch []error

func (enrollmentOutcomeBranch) Error() string { return "enrollment cause branch" }
func (branch enrollmentOutcomeBranch) Unwrap() []error {
	return []error(branch)
}

type failureMachineAuth struct {
	tokenErr error
	proofErr error
}

func (auth failureMachineAuth) Token(context.Context) (string, error) {
	if auth.tokenErr != nil {
		return "", auth.tokenErr
	}
	return "machine-token", nil
}

func (auth failureMachineAuth) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return nil, auth.proofErr
}

type journalStatusTransport struct {
	store       *FileCredentialStore
	failJournal bool
	status      int
}

func (transport journalStatusTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.failJournal {
		transport.store.failSave = 1
	}
	return &http.Response{
		StatusCode: transport.status,
		Status:     http.StatusText(transport.status),
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

func (transport responseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.err != nil {
		return nil, transport.err
	}
	response := *transport.response
	response.Request = request
	return &response, nil
}

type failingResponseBody struct {
	readErr  error
	closeErr error
}

func (body failingResponseBody) Read([]byte) (int, error) { return 0, body.readErr }
func (body failingResponseBody) Close() error             { return body.closeErr }

type closeFailureBody struct {
	reader   *strings.Reader
	closeErr error
	closed   int
}

func (body closeFailureBody) Read(destination []byte) (int, error) {
	return body.reader.Read(destination)
}
func (body *closeFailureBody) Close() error {
	body.closed++
	return body.closeErr
}

func assertSafePathFailure(t *testing.T, err error, path string, classification error) {
	t.Helper()
	if !errors.Is(err, classification) {
		t.Fatalf("failure=%v, want %v", err, classification)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("path cause was lost: %T %v", err, err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("failure exposed path: %v", err)
	}
}

func assertStaticFailureText(t *testing.T, err error, privateText string) {
	t.Helper()
	if strings.Contains(err.Error(), privateText) || strings.Contains(err.Error(), "control.example") {
		t.Fatalf("failure exposed private detail: %v", err)
	}
}

var _ io.ReadCloser = failingResponseBody{}
