package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat-server/internal/nativeprivateaccess"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestMachineServiceRuntimeProofPrecedesAuthority(t *testing.T) {
	body := `{"environment_id":"environment","resource_id":"machine","user_id":"caller","cli_client_session_id":"cli","access_session_id":"access","expected":{"schema":"invalid"}}`
	for _, tc := range []struct {
		name   string
		proof  bool
		denied bool
		status int
	}{{"missing proof", false, false, 401}, {"wrong proof", true, true, 401}, {"exact proof reaches validation", true, false, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			identity := &machineServiceProof{fakeRuntimeIdentity: &fakeRuntimeIdentity{}}
			if tc.denied {
				identity.err = controlplane.ErrHelperProof
			}
			r := httptest.NewRequest("POST", "/v1/runtime-machine-services/authorize", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer runtime")
			if tc.proof {
				r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
			}
			w := httptest.NewRecorder()
			runtimeMachineServiceAuthorize(identity, &nativeprivateaccess.Service{}).ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.proof && (identity.method != http.MethodPost || identity.path != "/v1/runtime-machine-services/authorize" || identity.machineID != "machine" || identity.projectID != "environment" || string(identity.body) != body) {
				t.Fatal("proof not bound to exact signed body and target")
			}
		})
	}
}

func TestMachineServiceErrorMapsOnlyPureExpectedFailures(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid", err: machineservices.ErrInvalid, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "conflict", err: machineservices.ErrConflict, wantStatus: http.StatusConflict, wantCode: "generation_conflict"},
		{name: "service denied", err: machineservices.ErrDenied, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "native access denied", err: nativeprivateaccess.ErrDenied, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "invalid with I/O failure", err: errors.Join(machineservices.ErrInvalid, syscall.EIO), wantStatus: http.StatusServiceUnavailable, wantCode: "machine_services_unavailable"},
		{name: "conflict with I/O failure", err: errors.Join(machineservices.ErrConflict, syscall.EIO), wantStatus: http.StatusServiceUnavailable, wantCode: "machine_services_unavailable"},
		{name: "denial with I/O failure", err: errors.Join(machineservices.ErrDenied, syscall.EIO), wantStatus: http.StatusServiceUnavailable, wantCode: "machine_services_unavailable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/machine-services", nil)
			response := httptest.NewRecorder()
			machineServiceError(response, request, test.err)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status=%d body=%s want status=%d code=%q", response.Code, response.Body.String(), test.wantStatus, test.wantCode)
			}
			if strings.Contains(response.Body.String(), "input/output error") {
				t.Fatalf("operational cause leaked in response: %s", response.Body.String())
			}
		})
	}
}

func TestMachineServiceMixedFailureIsCapturedAtHTTPBoundary(t *testing.T) {
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
	request := httptest.NewRequest(http.MethodPut, "/v1/machine-services", nil)
	request.Header.Set("Support-Reference", reference)
	response := httptest.NewRecorder()
	mixed := errors.Join(machineservices.ErrDenied, syscall.EIO)
	handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(io.Discard, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		machineServiceError(w, r, mixed)
	}))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "input/output error") {
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
			if record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusServiceUnavailable || record.Reference != reference || record.Outcome != "failed" {
				t.Fatalf("HTTP boundary lost operational cause or request reference: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("HTTP boundary did not capture the machine service operational cause")
}

func TestMachineServiceObservationCannotSilentlyDropSnapshot(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := `{"environment_id":"environment","resource_id":"machine","sampled_at":"` + now + `","machine_services":{"schema":"paperboat.machine-services/v1","installation_generation":1,"boot_id":"boot-012345678901","started_at":"` + now + `","generation":1,"services":[{"port":5432,"address_family":"ipv4"}]}}`
	identity := &fakeRuntimeIdentity{}
	repo := &fakeRuntimeObservationRepository{}
	r := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer runtime")
	r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	w := httptest.NewRecorder()
	runtimeObservation(repo, identity, 10).ServeHTTP(w, r)
	if w.Code != 503 || repo.recorded != nil || string(identity.body) != body {
		t.Fatalf("snapshot silently dropped: %d %s", w.Code, w.Body.String())
	}
}

func TestMachineServiceObservationRecordsSignedSnapshotAndReturnsPolicy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := machineservices.Snapshot{
		Schema: "paperboat.machine-services/v1", InstallationGeneration: 4,
		BootID: "boot-012345678901", StartedAt: now, Generation: 7,
		Services: []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}},
	}
	policy := machineservices.RuntimePolicy{
		Generation: 11, AutomaticDiscoveryEnabled: true,
		ExplicitPorts: []machineservices.Port{{Port: 8080, AddressFamily: "ipv6"}},
	}
	sink := &fakeMachineServicesRepository{policy: policy}
	repo := &fakeRuntimeObservationRepository{}
	identity := &fakeRuntimeIdentity{}
	body := machineServiceObservationBody(t, now, snapshot)
	r := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer runtime")
	addRuntimeProof(r)
	w := httptest.NewRecorder()
	runtimeObservation(repo, identity, 10, sink).ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if sink.recorded == nil || sink.environment != "environment" || sink.machine != "machine" || sink.observedAt != now || !snapshotsEqual(*sink.recorded, snapshot) {
		t.Fatalf("signed snapshot was not recorded exactly: %#v", sink)
	}
	if repo.recorded == nil || identity.projectID != "environment" || identity.machineID != "machine" || string(identity.body) != body {
		t.Fatal("heartbeat was not authenticated and metered for the exact signed target")
	}
	var response struct {
		Data struct {
			Policy machineservices.RuntimePolicy `json:"machine_services_policy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Policy.Generation != policy.Generation || !response.Data.Policy.AutomaticDiscoveryEnabled || len(response.Data.Policy.ExplicitPorts) != 1 || response.Data.Policy.ExplicitPorts[0] != policy.ExplicitPorts[0] {
		t.Fatalf("response policy=%#v want=%#v", response.Data.Policy, policy)
	}
}

func TestMachineServiceObservationRejectedSnapshotIsNotStoredOrMetered(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{{"invalid", machineservices.ErrInvalid, http.StatusBadRequest}, {"revoked", machineservices.ErrDenied, http.StatusForbidden}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}}
			sink := &fakeMachineServicesRepository{recordErr: tc.err}
			repo := &fakeRuntimeObservationRepository{}
			r := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(machineServiceObservationBody(t, now, snapshot)))
			r.Header.Set("Authorization", "Bearer runtime")
			addRuntimeProof(r)
			w := httptest.NewRecorder()
			runtimeObservation(repo, &fakeRuntimeIdentity{}, 10, sink).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if sink.recorded != nil || sink.policyCalls != 0 || repo.recorded != nil {
				t.Fatalf("rejected snapshot changed state: sink=%#v metered=%#v", sink, repo.recorded)
			}
		})
	}
}

func TestMachineServiceObservationMixedFailureRetainsCauseAtHTTPBoundary(t *testing.T) {
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	snapshot := machineservices.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []machineservices.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	sink := &fakeMachineServicesRepository{recordErr: errors.Join(machineservices.ErrInvalid, syscall.EIO)}
	repo := &fakeRuntimeObservationRepository{}
	identity := &fakeRuntimeIdentity{}
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(machineServiceObservationBody(t, now, snapshot)))
	request.Header.Set("Authorization", "Bearer runtime")
	addRuntimeProof(request)
	reference := supportreport.NewReference()
	request.Header.Set("Support-Reference", reference)
	response := httptest.NewRecorder()
	handler := supportMiddleware(reports, slog.New(slog.NewTextHandler(io.Discard, nil)), runtimeObservation(repo, identity, 10, sink))
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "input/output error") {
		t.Fatalf("mixed failure response=%d %s", response.Code, response.Body.String())
	}
	if sink.recorded != nil || sink.policyCalls != 0 || repo.recorded != nil {
		t.Fatalf("failed machine service snapshot changed or metered state: sink=%#v metered=%#v", sink, repo.recorded)
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
			if record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusServiceUnavailable || record.Reference != reference || record.Outcome != "failed" {
				t.Fatalf("HTTP boundary lost machine service cause or request reference: %+v", record)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("HTTP boundary did not capture the machine service operational cause")
}

type fakeMachineServicesRepository struct {
	policy               machineservices.RuntimePolicy
	recordErr            error
	recorded             *machineservices.Snapshot
	environment, machine string
	observedAt           time.Time
	policyCalls          int
}

func (f *fakeMachineServicesRepository) RecordMachineServices(_ context.Context, environment, machine string, snapshot machineservices.Snapshot, observedAt time.Time) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.environment, f.machine, f.observedAt = environment, machine, observedAt
	f.recorded = &snapshot
	return nil
}

func (f *fakeMachineServicesRepository) RuntimePolicy(context.Context, string, string) (machineservices.RuntimePolicy, error) {
	f.policyCalls++
	return f.policy, nil
}

func machineServiceObservationBody(t *testing.T, sampledAt time.Time, snapshot machineservices.Snapshot) string {
	t.Helper()
	body, err := json.Marshal(struct {
		EnvironmentID   string                   `json:"environment_id"`
		ResourceID      string                   `json:"resource_id"`
		SampledAt       time.Time                `json:"sampled_at"`
		MachineServices machineservices.Snapshot `json:"machine_services"`
	}{"environment", "machine", sampledAt, snapshot})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func snapshotsEqual(a, b machineservices.Snapshot) bool {
	return a.Schema == b.Schema && a.InstallationGeneration == b.InstallationGeneration && a.BootID == b.BootID && a.StartedAt.Equal(b.StartedAt) && a.Generation == b.Generation && len(a.Services) == 1 && len(b.Services) == 1 && a.Services[0] == b.Services[0]
}

type machineServiceProof struct {
	*fakeRuntimeIdentity
	method, path string
}

func (f *machineServiceProof) VerifyMachineRequest(ctx context.Context, token string, proof []byte, method, path string, body []byte) (controlplane.MachineRequestClaims, error) {
	f.method, f.path = method, path
	var in struct {
		EnvironmentID string `json:"environment_id"`
		ResourceID    string `json:"resource_id"`
	}
	_ = json.Unmarshal(body, &in)
	err := f.VerifyRuntimeObservation(ctx, token, proof, body, in.EnvironmentID, in.ResourceID)
	return controlplane.MachineRequestClaims{EnvironmentID: in.EnvironmentID, MachineID: in.ResourceID}, err
}

func TestMachineServiceRuntimeProofRequiresExactMachineAndEnvironment(t *testing.T) {
	body := `{"environment_id":"environment","resource_id":"machine","user_id":"caller","cli_client_session_id":"cli","access_session_id":"access","expected":{"schema":"invalid"}}`
	for _, identity := range []controlplane.MachineRequestClaims{{EnvironmentID: "other", MachineID: "machine"}, {EnvironmentID: "environment", MachineID: "other"}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/runtime-machine-services/authorize", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer runtime")
		r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
		w := httptest.NewRecorder()
		runtimeMachineServiceAuthorize(machineServiceWrongIdentity{identity}, &nativeprivateaccess.Service{}).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("mismatched runtime identity reached authority: %d", w.Code)
		}
	}
}

type machineServiceWrongIdentity struct {
	identity controlplane.MachineRequestClaims
}

func (v machineServiceWrongIdentity) VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
	return v.identity, nil
}
