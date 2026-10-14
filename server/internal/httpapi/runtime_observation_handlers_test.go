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
	"github.com/pinksaucepasta/paperboat-server/internal/metering"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type fakeRuntimeObservationRepository struct {
	verifyErr error
	recordErr error
	recorded  *metering.RuntimeObservation
	observed  []metering.RuntimeObservation
}

type fakeRuntimeIdentity struct {
	token, projectID, machineID string
	proof, body                 []byte
	err                         error
}

type fakeUpdateObservationRepository struct {
	recorded *usermachines.UpdateObservation
	err      error
	errs     []error
}

type fakeAvailabilityObservationRepository struct {
	recorded *usermachines.AvailabilityObservation
	err      error
}

func (f *fakeAvailabilityObservationRepository) RecordAvailabilityObservation(_ context.Context, _, _ string, observation usermachines.AvailabilityObservation) error {
	f.recorded = &observation
	return f.err
}

func (f *fakeUpdateObservationRepository) RecordUpdateObservation(_ context.Context, _, _ string, observation usermachines.UpdateObservation) error {
	f.recorded = &observation
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return f.err
}

func (f *fakeRuntimeIdentity) VerifyRuntimeObservation(_ context.Context, token string, proof, body []byte, projectID, machineID string) error {
	f.token, f.proof, f.body, f.projectID, f.machineID = token, append([]byte(nil), proof...), append([]byte(nil), body...), projectID, machineID
	return f.err
}

func addRuntimeProof(request *http.Request) {
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("machine-proof")))
}

func (f *fakeRuntimeObservationRepository) VerifyHeartbeatCredential(context.Context, string, string, string) error {
	return f.verifyErr
}

func (f *fakeRuntimeObservationRepository) RecordRuntimeObservation(_ context.Context, observation metering.RuntimeObservation) error {
	f.recorded = &observation
	f.observed = append(f.observed, observation)
	return f.recordErr
}

func TestRuntimeObservationRejectsObsoleteConfigStatus(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{}
	body := `{
		"environment_id":"prj_test","resource_id":"machine_test",
		"sampled_at":"2026-07-14T01:00:01Z",
		"reporter_version":"test",
		"config_sync":{"state":"error","pending_path_count":3,
		"skipped":[{"path":".config/a","bytes":6,"reason":"Too Large"},{"path":".config/b","bytes":7,"reason":"Too Large"},{"path":".config/c","bytes":8,"reason":"Too Large"}],
		"conflicts":[],"error_code":"Git Auth Failed!","error_message":"request https://example.test/?token=secret failed",
		"max_file_bytes":10,"max_batch_bytes":20,"policy_revision":"revision-one","updated_at":"2026-07-14T01:00:00Z"}
	}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	runtimeObservation(repository, nil, 2).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if repository.recorded != nil {
		t.Fatal("obsolete config sync heartbeat was recorded")
	}
}

func TestRuntimeObservationUsesProofBoundHelperIdentity(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{verifyErr: errors.New("legacy verifier must not run")}
	identity := &fakeRuntimeIdentity{}
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-07-14T01:00:01Z"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer helper-identity")
	request.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
	recorder := httptest.NewRecorder()
	runtimeObservation(repository, identity, 10).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || identity.token != "helper-identity" || string(identity.proof) != "proof" || string(identity.body) != body || identity.projectID != "prj_test" || identity.machineID != "machine_test" {
		t.Fatalf("status=%d identity=%#v", recorder.Code, identity)
	}
}

func TestRuntimeObservationRecordsSignedUpdateObservation(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{}
	updates := &fakeUpdateObservationRepository{}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-06T12:00:01Z","update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.18.1","channel":"stable","operation_id":"update-op-0001","installation_generation":2,"worker_generation":4,"os_boot_id":"boot-1","rollback_count":0,"observed_at":"` + now.Format(time.RFC3339) + `"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	recorder := httptest.NewRecorder()
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10, updates).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || updates.recorded == nil || !strings.Contains(recorder.Body.String(), `"update_observation_recorded":true`) {
		t.Fatalf("status=%d update=%#v body=%s", recorder.Code, updates.recorded, recorder.Body.String())
	}
}

func TestRuntimeObservationIgnoresStaleUpdateObservation(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{}
	updates := &fakeUpdateObservationRepository{err: usermachines.ErrUpdateObservationStale}
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-06T12:00:01Z","update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.18.1","channel":"stable","operation_id":"update-op-0001","installation_generation":2,"worker_generation":4,"os_boot_id":"boot-1","rollback_count":0,"observed_at":"2026-08-06T12:00:00Z"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	recorder := httptest.NewRecorder()
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10, updates).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"update_observation_recorded":false`) || !strings.Contains(recorder.Body.String(), `"code":"update_observation_stale"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRuntimeObservationKeepsHeartbeatAcceptedAcrossFailedThenFreshUpdate(t *testing.T) {
	const oldFailed = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","runtime_diagnostics":{"capabilities":[],"worker_generation":4,"os_boot_id":"boot-old","worker_service_scope":"system","connector_state":"ready","connector_generation":2,"observed_at":"2026-08-29T12:00:01Z"},"update":{"schema":"paperboat.update-observation/v1","state":"failed","current_version":"2026.08.29.1","target_version":"2026.08.29.1","channel":"stable","operation_id":"update-old-failed","installation_generation":2,"worker_generation":4,"os_boot_id":"boot-old","rollback_count":0,"error_code":"update_failed","observed_at":"2026-08-29T12:00:00Z"}}`
	const freshWorker = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:16Z","runtime_diagnostics":{"capabilities":[],"worker_generation":5,"os_boot_id":"boot-new","worker_service_scope":"system","connector_state":"ready","connector_generation":2,"observed_at":"2026-08-29T12:00:16Z"},"update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.29.1","channel":"stable","operation_id":"update-fresh-worker","installation_generation":2,"worker_generation":5,"os_boot_id":"boot-new","rollback_count":0,"observed_at":"2026-08-29T12:00:15Z"}}`
	repository := &fakeRuntimeObservationRepository{}
	updates := &fakeUpdateObservationRepository{errs: []error{nil, errors.New("database connection refused")}}
	handler := runtimeObservation(repository, &fakeRuntimeIdentity{}, 10, updates)
	responses := make([]string, 0, 2)
	for _, body := range []string{oldFailed, freshWorker} {
		request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer machine-token")
		addRuntimeProof(request)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"accepted":true`) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		responses = append(responses, recorder.Body.String())
	}
	if len(repository.observed) != 2 || updates.recorded == nil || updates.recorded.WorkerGeneration != 5 {
		t.Fatalf("observed=%#v last update=%#v", repository.observed, updates.recorded)
	}
	if !strings.Contains(responses[0], `"update_observation_recorded":true`) {
		t.Fatalf("old failed update was not recorded: %s", responses[0])
	}
	if !strings.Contains(responses[1], `"update_observation_recorded":false`) || !strings.Contains(responses[1], `"code":"update_observation_unavailable"`) {
		t.Fatalf("fresh worker update storage failure was not isolated: %s", responses[1])
	}
}

func TestRuntimeObservationRecordsPresenceWhenAuxiliaryObservationIsRejected(t *testing.T) {
	const (
		availabilityBody = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","availability":{"schema":"paperboat.availability-policy/v1","mode":"allow_sleep","version":1,"status":"applied","observed_at":"2026-08-29T12:00:00Z","host_service_version":"2026.08.29.1","host_service_scope":"system","update_rollbacks":0,"update_health":"healthy"}}`
		updateBody       = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-28T12:00:01Z","update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.29.1","channel":"stable","operation_id":"update-op-0001","installation_generation":2,"worker_generation":4,"os_boot_id":"boot-1","rollback_count":0,"observed_at":"2026-08-28T12:00:00Z"}}`
		invalidUpdate    = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-28T12:00:01Z","update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.29.1","channel":"stable","operation_id":"update-op-0001","installation_generation":0,"worker_generation":4,"os_boot_id":"boot-1","rollback_count":0,"observed_at":"2026-08-28T12:00:00Z"}}`
	)
	tests := []struct {
		name     string
		body     string
		sink     any
		wantCode string
	}{
		{name: "stale availability", body: availabilityBody, sink: &fakeAvailabilityObservationRepository{err: usermachines.ErrAvailabilityObservationStale}, wantCode: "availability_observation_stale"},
		{name: "invalid availability", body: availabilityBody, sink: &fakeAvailabilityObservationRepository{err: usermachines.ErrAvailabilityInvalid}, wantCode: "availability_observation_invalid"},
		{name: "stale update", body: updateBody, sink: &fakeUpdateObservationRepository{err: usermachines.ErrUpdateObservationStale}, wantCode: "update_observation_stale"},
		{name: "invalid update", body: invalidUpdate, wantCode: "update_observation_invalid"},
		{name: "conflicting update", body: updateBody, sink: &fakeUpdateObservationRepository{err: usermachines.ErrUpdateObservationConflict}, wantCode: "update_observation_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeRuntimeObservationRepository{}
			request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer machine-token")
			addRuntimeProof(request)
			recorder := httptest.NewRecorder()
			if test.sink == nil {
				runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(recorder, request)
			} else {
				runtimeObservation(repository, &fakeRuntimeIdentity{}, 10, test.sink).ServeHTTP(recorder, request)
			}
			if recorder.Code != http.StatusAccepted || repository.recorded == nil || !strings.Contains(recorder.Body.String(), `"accepted":true`) || !strings.Contains(recorder.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status=%d observation=%#v body=%s", recorder.Code, repository.recorded, recorder.Body.String())
			}
		})
	}
}

func TestRuntimeObservationAcceptsHeartbeatWhenOptionalSinksAreMissing(t *testing.T) {
	const (
		availabilityBody = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","availability":{"schema":"paperboat.availability-policy/v1","mode":"allow_sleep","version":1,"status":"applied","observed_at":"2026-08-29T12:00:00Z","host_service_version":"2026.08.29.1","host_service_scope":"system","update_rollbacks":0,"update_health":"healthy"}}`
		capabilitiesBody = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","machine_capabilities":{"schema":"paperboat.machine-capabilities/v1","version":1,"applied":{"terminal":true},"status":"applied","observed_at":"2026-08-29T12:00:00Z"}}`
		updateBody       = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","update":{"schema":"paperboat.update-observation/v1","state":"healthy","current_version":"2026.08.29.1","channel":"stable","operation_id":"update-op-0001","installation_generation":2,"worker_generation":4,"os_boot_id":"boot-1","rollback_count":0,"observed_at":"2026-08-29T12:00:00Z"}}`
	)

	for _, tc := range []struct{ name, body, code string }{
		{name: "availability", body: availabilityBody, code: "availability_observation_unavailable"},
		{name: "machine capabilities", body: capabilitiesBody, code: "capabilities_observation_unavailable"},
		{name: "update", body: updateBody, code: "update_observation_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := &fakeRuntimeObservationRepository{}
			request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer machine-token")
			addRuntimeProof(request)
			response := httptest.NewRecorder()
			runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(response, request)
			if response.Code != http.StatusAccepted || repository.recorded == nil || !strings.Contains(response.Body.String(), `"accepted":true`) || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d observation=%#v body=%s", response.Code, repository.recorded, response.Body.String())
			}
		})
	}
}

func TestRuntimeObservationReportsAuxiliaryStorageCauseWithoutFailingHeartbeat(t *testing.T) {
	const body = `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-08-29T12:00:01Z","availability":{"schema":"paperboat.availability-policy/v1","mode":"allow_sleep","version":1,"status":"applied","observed_at":"2026-08-29T12:00:00Z","host_service_version":"2026.08.29.1","host_service_scope":"system","update_rollbacks":0,"update_health":"healthy"}}`
	store := &supportTestStore{}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reporterCtx, cancelReporter := context.WithCancel(context.Background())
	reporterDone := make(chan struct{})
	go func() { _ = reporter.Run(reporterCtx); close(reporterDone) }()
	defer func() {
		cancelReporter()
		<-reporterDone
	}()

	repository := &fakeRuntimeObservationRepository{}
	availability := &fakeAvailabilityObservationRepository{err: errors.Join(syscall.ECONNREFUSED, errors.New("DB_SECRET_CANARY"))}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := requestID(correlationID(supportMiddleware(reporter, logger, runtimeObservation(repository, &fakeRuntimeIdentity{}, 10, availability))))
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || repository.recorded == nil || !strings.Contains(response.Body.String(), `"code":"availability_observation_unavailable"`) {
		t.Fatalf("heartbeat did not remain accepted with an auxiliary rejection: status=%d body=%s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		if len(store.records) > 0 {
			record := store.records[0]
			store.mu.Unlock()
			if record.Code != "availability_observation_unavailable" || record.Operation != "http_machines" || record.Stage != "service_error" || record.Cause != "connection_refused" || record.HTTPStatus != 0 || record.Reference != response.Header().Get("Support-Reference") || record.CorrelationID != response.Header().Get("Correlation-Id") || record.RequestID == "" || record.RequestID != response.Header().Get("Request-Id") {
				t.Fatalf("auxiliary fault metadata lost or mismatched: %+v", record)
			}
			encoded, err := json.Marshal(record)
			if err != nil || strings.Contains(string(encoded), "DB_SECRET_CANARY") {
				t.Fatalf("private storage error escaped into the support record: %s (%v)", encoded, err)
			}
			return
		}
		store.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("auxiliary storage cause was not retained in the request-owned support reporter")
}

func TestRuntimeObservationRejectsUnsafeSummaryAndWrongCredential(t *testing.T) {
	validPrefix := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-07-14T01:00:01Z","config_sync":`
	missingTimestamp := validPrefix + `{"state":"healthy","pending_path_count":0,"max_file_bytes":10,"max_batch_bytes":20,"policy_revision":"1"}}`
	repository := &fakeRuntimeObservationRepository{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(missingTimestamp))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || repository.recorded != nil {
		t.Fatalf("missing status timestamp = %d recorded=%v", recorder.Code, repository.recorded != nil)
	}

	unsafe := validPrefix + `{"state":"warning","pending_path_count":1,"skipped":[{"path":"../secret","reason":"unsafe"}],"conflicts":[],"max_file_bytes":10,"max_batch_bytes":20,"policy_revision":"1","updated_at":"2026-07-14T01:00:00Z"}}`
	repository = &fakeRuntimeObservationRepository{}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(unsafe))
	request.Header.Set("Authorization", "Bearer machine-token")
	runtimeObservation(repository, nil, 10).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || repository.recorded != nil {
		t.Fatalf("unsafe summary status = %d recorded=%v", recorder.Code, repository.recorded != nil)
	}

}

func TestRuntimeObservationRepositoryFailureIsInternalError(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{recordErr: errors.New("database unavailable")}
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-07-14T01:00:01Z"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("repository failure status = %d", recorder.Code)
	}
}

func TestRuntimeObservationReportsDuplicateMachineIdentity(t *testing.T) {
	repository := &fakeRuntimeObservationRepository{recordErr: metering.ErrDuplicateMachineIdentity}
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(`{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-07-14T01:00:01Z"}`))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	recorder := httptest.NewRecorder()
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"duplicate_machine_identity"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRuntimeObservationDuplicateConflictWithOperationalCauseReportsFailure(t *testing.T) {
	const private = "RUNTIME_OBSERVATION_PRIVATE_DATABASE_DETAIL"
	store := &supportTestStore{}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reporterCtx, cancelReporter := context.WithCancel(context.Background())
	reporterDone := make(chan struct{})
	go func() { _ = reporter.Run(reporterCtx); close(reporterDone) }()
	defer func() {
		cancelReporter()
		<-reporterDone
	}()

	reference := supportreport.NewReference()
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(`{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-07-14T01:00:01Z"}`))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Support-Reference", reference)
	request.Header.Set("Correlation-Id", "correlation_metering_01")
	request.Header.Set("Request-Id", "request_metering_01")
	addRuntimeProof(request)

	var logs strings.Builder
	err = errors.Join(metering.ErrDuplicateMachineIdentity, syscall.EIO, errors.New(private))
	repository := &fakeRuntimeObservationRepository{recordErr: err}
	base := runtimeObservation(repository, &fakeRuntimeIdentity{}, 10)
	handler := requestID(correlationID(supportMiddleware(reporter, slog.New(slog.NewJSONHandler(&logs, nil)), base)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal_error"`) || strings.Contains(response.Body.String(), private) {
		t.Fatalf("mixed runtime failure response=%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Support-Reference") != reference {
		t.Fatal("runtime failure response lost its support reference")
	}

	var event struct {
		Operation        string `json:"operation"`
		Stage            string `json:"stage"`
		Code             string `json:"code"`
		Cause            string `json:"cause"`
		Errno            int    `json:"errno"`
		HTTPStatus       int    `json:"http_status"`
		SupportReference string `json:"support_reference"`
		CorrelationID    string `json:"correlation_id"`
		RequestID        string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &event); err != nil {
		t.Fatalf("expected exactly one structured failure event: %v; logs=%q", err, logs.String())
	}
	if event.Operation != "http_machines" || event.Stage != "http_handler" || event.Code != "internal_error" || event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.HTTPStatus != http.StatusInternalServerError || event.SupportReference != reference || event.CorrelationID != "correlation_metering_01" || event.RequestID != "request_metering_01" {
		t.Fatalf("failure event lost the operational cause or request identity: %#v", event)
	}
	if strings.Contains(logs.String(), private) {
		t.Fatal("failure log disclosed private storage detail")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		records := append([]supportreport.Record(nil), store.records...)
		store.mu.Unlock()
		if len(records) > 0 {
			if len(records) != 1 {
				t.Fatalf("captured %d support records, want one", len(records))
			}
			record := records[0]
			if record.Code != "internal_error" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusInternalServerError || record.Reference != reference || record.CorrelationID != "correlation_metering_01" || record.RequestID != "request_metering_01" || record.Outcome != "failed" {
				t.Fatalf("support record lost the original operational cause or request identity: %+v", record)
			}
			encoded, err := json.Marshal(record)
			if err != nil || strings.Contains(string(encoded), private) {
				t.Fatalf("private failure detail escaped into the support record: %s (%v)", encoded, err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime observation operational cause was not captured")
}

func TestNormalizeConfigStatusTimestampPreservesAgeAcrossClockSkew(t *testing.T) {
	serverNow := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	for _, skew := range []time.Duration{-time.Hour, time.Hour} {
		sampledAt := serverNow.Add(skew)
		statusUpdated := sampledAt.Add(-5 * time.Minute)
		ordered, observed, valid := normalizeStatusTimestamps(statusUpdated, sampledAt, serverNow)
		if !valid || !observed.Equal(serverNow.Add(-5*time.Minute)) {
			t.Fatalf("skew %s: observed timestamp = %s valid=%v", skew, observed, valid)
		}
		if skew < 0 && !ordered.Equal(statusUpdated) {
			t.Fatalf("clock-behind ordering timestamp = %s, want source %s", ordered, statusUpdated)
		}
		if skew > 0 && !ordered.Equal(observed) {
			t.Fatalf("clock-ahead ordering timestamp = %s, want corrected %s", ordered, observed)
		}
	}
}

func TestValidObservedCapabilitiesRejectsUnknownDuplicatesAndMachineRelay(t *testing.T) {
	if !validObservedCapabilities([]string{"file_receive", "preview_launch"}) {
		t.Fatal("client capabilities should be valid")
	}
	for _, capabilities := range [][]string{{"terminal"}, {"file_receive", "file_receive"}, {"peer_relay"}} {
		if validObservedCapabilities(capabilities) {
			t.Fatalf("capabilities %v should be invalid", capabilities)
		}
	}
}

type fakeTerminalJoinRepository struct {
	calls                int
	environment, machine string
	join                 usermachines.TerminalJoinObservation
	err                  error
}

func (s *fakeTerminalJoinRepository) RecordTerminalJoin(_ context.Context, environment, machine string, in usermachines.TerminalJoinObservation) error {
	s.calls++
	s.environment = environment
	s.machine = machine
	s.join = in
	return s.err
}
func TestRuntimeTerminalJoinRequiresSignedHostAndDurableAudit(t *testing.T) {
	body := `{"environment_id":"env_a","resource_id":"machine_a","sampled_at":"2026-09-12T10:00:00Z","terminal_join":{"access_session_id":"access_a","terminal_session_id":"session_a","attachment_id":"att_a"}}`
	for _, tc := range []struct {
		name                 string
		proof                bool
		authErr, errorResult error
		status               int
	}{
		{"legacy token", false, nil, nil, 401}, {"invalid proof", true, controlplane.ErrHelperProof, nil, 401},
		{"revoked", true, nil, usermachines.ErrTerminalSessionNotFound, 403}, {"storage failure", true, nil, errors.New("storage unavailable"), 503}, {"recorded", true, nil, nil, 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRuntimeObservationRepository{}
			joins := &fakeTerminalJoinRepository{err: tc.errorResult}
			identity := &fakeRuntimeIdentity{err: tc.authErr}
			request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer token")
			if tc.proof {
				request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
			}
			w := httptest.NewRecorder()
			runtimeObservation(repo, identity, 10, joins).ServeHTTP(w, request)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			if repo.recorded != nil {
				t.Fatal("join altered heartbeat state")
			}
			if tc.status == 202 && (joins.calls != 1 || joins.environment != "env_a" || joins.machine != "machine_a" || joins.join.AccessSessionID != "access_a") {
				t.Fatal("lost host/access binding")
			}
			if (tc.status == 401 || !tc.proof) && joins.calls != 0 {
				t.Fatal("unauthenticated join reached storage")
			}
		})
	}
}

func TestRuntimeEnvironmentCapabilityUsesSeparateLayerObservationEndpoint(t *testing.T) {
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-10-08T12:00:01Z","runtime_diagnostics":{"capabilities":["environment_injection"],"worker_generation":5,"os_boot_id":"boot-new","worker_service_scope":"system","connector_state":"ready","connector_generation":2,"observed_at":"2026-10-08T12:00:01Z"}}`
	repository := &fakeRuntimeObservationRepository{}
	identity := &fakeRuntimeIdentity{}
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	response := httptest.NewRecorder()
	runtimeObservation(repository, identity, 10).ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || repository.recorded == nil || string(identity.body) != body || identity.machineID != "machine_test" || identity.projectID != "prj_test" {
		t.Fatalf("layer-era signed heartbeat rejected: status=%d", response.Code)
	}
	if len(repository.recorded.Capabilities) != 1 || repository.recorded.Capabilities[0] != "environment_injection" {
		t.Fatal("ENV capability lost")
	}
	if strings.Contains(response.Body.String(), "environment_bundle") {
		t.Fatal("heartbeat returned obsolete projection")
	}
}
func TestRuntimeObservationRejectsRemovedProjectionFieldWithoutLeakingValues(t *testing.T) {
	const canary = "ENV_CANARY_RUNTIME_PLAINTEXT_MUST_NOT_CROSS"
	body := `{"environment_id":"prj_test","resource_id":"machine_test","sampled_at":"2026-10-08T12:00:01Z","environment":{"value":"` + canary + `"}}`
	repository := &fakeRuntimeObservationRepository{}
	request := httptest.NewRequest(http.MethodPost, "/v1/runtime-observations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	addRuntimeProof(request)
	response := httptest.NewRecorder()
	runtimeObservation(repository, &fakeRuntimeIdentity{}, 10).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || repository.recorded != nil || strings.Contains(response.Body.String(), canary) {
		t.Fatalf("obsolete private projection field accepted or leaked: status=%d", response.Code)
	}
}
