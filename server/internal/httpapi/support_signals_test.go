package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func TestSupportSignalsConnectedHTTPOutcomesAndParent(t *testing.T) {
	var mu sync.Mutex
	var envelopes []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		envelopes = append(envelopes, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()
	rate := 1.0
	store := &supportTestStore{}
	reports, err := supportreport.New(store, supportreport.Config{Enabled: true, DSN: strings.Replace(sink.URL, "://", "://public@", 1) + "/1", Release: "server-http-test", Environment: "test", TracesSampleRate: &rate})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { reports.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/billing/success", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /v1/billing/denied", func(w http.ResponseWriter, r *http.Request) { writeError(w, r, 403, "forbidden", "Not allowed.") })
	mux.HandleFunc("GET /v1/billing/failed", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 500, "internal_error", "The operation failed.")
	})
	mux.HandleFunc("GET /v1/billing/unavailable", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 503, "provider_unavailable", "The provider is unavailable.", syscall.ETIMEDOUT)
	})
	mux.HandleFunc("GET /v1/billing/not-implemented", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 501, "internal_error", "The operation is unavailable.", errors.New("SECRET_CAUSE"))
	})
	mux.HandleFunc("GET /v1/billing/panic", func(w http.ResponseWriter, r *http.Request) { panic("SECRET_PANIC") })
	mux.HandleFunc("GET /v1/auth/proof-outage", func(w http.ResponseWriter, r *http.Request) {
		writeMachineProofFailure(w, r, fmt.Errorf("database password SECRET_PROOF_DB_URL: %w", syscall.ECONNRESET), "machine_identity_invalid", "Machine identity proof was rejected.")
	})
	mux.HandleFunc("GET /v1/auth/proof-rejected", func(w http.ResponseWriter, r *http.Request) {
		writeMachineProofFailure(w, r, controlplane.ErrHelperProof, "machine_identity_invalid", "Machine identity proof was rejected.")
	})
	mux.HandleFunc("GET /v1/auth/proof-rejected-wrapped", func(w http.ResponseWriter, r *http.Request) {
		writeMachineProofFailure(w, r, fmt.Errorf("wrapped verifier rejection: %w", controlplane.ErrHelperProof), "machine_identity_invalid", "Machine identity proof was rejected.")
	})
	mux.HandleFunc("GET /v1/auth/proof-mixed-outage", func(w http.ResponseWriter, r *http.Request) {
		writeMachineProofFailure(w, r, errors.Join(controlplane.ErrHelperProof, syscall.EIO), "machine_identity_invalid", "Machine identity proof was rejected.")
	})
	mux.HandleFunc("GET /v1/config/assignment-outage", func(w http.ResponseWriter, r *http.Request) {
		cause := fmt.Errorf("database credential SECRET_CONFIG_DB: %w", syscall.ECONNRESET)
		writeConfigAssignmentFailure(w, r, cause, "Configuration assignment is temporarily unavailable.")
	})
	mux.HandleFunc("GET /v1/config/team-default-outage", func(w http.ResponseWriter, r *http.Request) {
		cause := fmt.Errorf("database credential SECRET_TEAM_CONFIG_DB: %w", syscall.ECONNRESET)
		configDefaultError(w, r, cause)
	})
	mux.HandleFunc("GET /v1/config/conflict-outage", func(w http.ResponseWriter, r *http.Request) {
		cause := fmt.Errorf("database credential SECRET_CONFLICT_DB: %w", syscall.ECONNRESET)
		writeConfigConflictRequestError(w, r, cause, "conflict_resolution_invalid", "conflict_resolution_stale", "Conflict resolution could not be requested.")
	})
	mux.HandleFunc("GET /v1/config/repository-read-access-outage", func(w http.ResponseWriter, r *http.Request) {
		cause := errors.Join(controlplane.ErrConfigRepositoryAccessUncertain, fmt.Errorf("provider credential SECRET_READ_ACCESS_TOKEN: %w", syscall.ECONNRESET))
		writeConfigRepositoryReadAccessError(w, r, cause)
	})
	router := NewRouter(Options{OverrideHandler: mux, SupportReports: reports, Config: config.Config{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	api := httptest.NewServer(router)
	defer api.Close()
	ref := supportreport.NewReference()
	trace := strings.Repeat("a", 32)
	parent := strings.Repeat("b", 16)
	for _, scenario := range []struct {
		path   string
		status int
	}{{"success", 204}, {"denied", 403}, {"failed", 500}, {"unavailable", 503}, {"not-implemented", 501}, {"panic", 500}, {"proof-outage", 503}, {"proof-rejected", 401}, {"proof-rejected-wrapped", 401}, {"proof-mixed-outage", 503}, {"assignment-outage", 503}, {"team-default-outage", 503}, {"conflict-outage", 503}, {"repository-read-access-outage", 503}} {
		path := "/v1/billing/" + scenario.path
		if strings.HasPrefix(scenario.path, "proof-") {
			path = "/v1/auth/" + scenario.path
		} else if scenario.path == "assignment-outage" {
			path = "/v1/config/" + scenario.path
		} else if scenario.path == "team-default-outage" {
			path = "/v1/config/" + scenario.path
		} else if scenario.path == "conflict-outage" {
			path = "/v1/config/" + scenario.path
		} else if scenario.path == "repository-read-access-outage" {
			path = "/v1/config/" + scenario.path
		}
		req, _ := http.NewRequest("GET", api.URL+path+"?token=SECRET_QUERY", nil)
		req.Header.Set("Support-Reference", ref)
		req.Header.Set("sentry-trace", trace+"-"+parent+"-1")
		req.Header.Set("Authorization", "Bearer SECRET_AUTH")
		response, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != scenario.status || response.Header.Get("Support-Reference") != ref {
			t.Fatalf("%s: %d reference %s", scenario.path, response.StatusCode, response.Header.Get("Support-Reference"))
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reporter did not drain")
	}
	store.mu.Lock()
	records := append([]supportreport.Record(nil), store.records...)
	store.mu.Unlock()
	var unavailable *supportreport.Record
	for i := range records {
		if records[i].Code == "provider_unavailable" {
			unavailable = &records[i]
		}
		if records[i].Code == "forbidden" {
			t.Fatal("expected HTTP rejection was unexpectedly captured")
		}
		if records[i].Code == "authorization_unavailable" && records[i].Cause == "connection_reset" {
			if records[i].HTTPStatus != 503 || records[i].CorrelationID != ref {
				t.Fatalf("proof outage reference/status was not retained: %+v", records[i])
			}
		}
		if records[i].Code == "authorization_unavailable" && records[i].Errno == int(syscall.EIO) {
			if records[i].Cause != "system_error" || records[i].HTTPStatus != 503 || records[i].CorrelationID != ref || records[i].Outcome != "failed" || !strings.Contains(strings.Join(records[i].ErrorChain, ","), "syscall.Errno") {
				t.Fatalf("mixed proof and I/O failure lost its original bounded cause/status: %+v", records[i])
			}
		}
		if records[i].Code == "configuration_unavailable" {
			if records[i].Cause != "connection_reset" || records[i].HTTPStatus != 503 || records[i].CorrelationID != ref {
				t.Fatalf("configuration/conflict outage cause/reference/status was not retained: %+v", records[i])
			}
		}
		if records[i].Code == "access_uncertain" {
			if records[i].Cause != "connection_reset" || records[i].HTTPStatus != 503 || records[i].CorrelationID != ref {
				t.Fatalf("repository read-access uncertainty cause/reference/status was not retained: %+v", records[i])
			}
		}
		if records[i].Code == "configuration_default_unavailable" {
			if records[i].Cause != "connection_reset" || records[i].HTTPStatus != 503 || records[i].CorrelationID != ref {
				t.Fatalf("team default outage cause/reference/status was not retained: %+v", records[i])
			}
		}
		if records[i].Code == "machine_identity_invalid" {
			t.Fatal("expected machine proof rejection was unexpectedly captured")
		}
	}
	if unavailable == nil || unavailable.Cause != "network_timeout" || unavailable.Severity != "error" || unavailable.HTTPStatus != 503 || unavailable.CorrelationID != ref {
		t.Fatalf("503 diagnostic cause/reference was not stored: %+v", records)
	}
	if unavailable.SourceFile == "" || strings.ContainsAny(unavailable.SourceFile, "/\\") || unavailable.SourceFunction == "" || strings.Contains(unavailable.SourceFunction, "/") || unavailable.SourceLine < 1 {
		t.Fatalf("support record has no safe source identity: %+v", unavailable)
	}
	mu.Lock()
	payload := strings.Join(envelopes, "\n")
	mu.Unlock()
	for _, v := range []string{trace, parent, ref, "http_billing", `"outcome":"success"`, `"outcome":"rejected"`, `"outcome":"failed"`, `"cause":"upstream_failure"`, `"cause":"network_timeout"`, `"http_status":"501"`, `"http_status":"503"`, `"source_file"`, `"source_function"`, `"source_line"`, "panic"} {
		if !strings.Contains(payload, v) {
			t.Errorf("missing %s in %s", v, payload)
		}
	}
	if strings.Contains(payload, "SECRET_") {
		t.Fatal("HTTP instrumentation leaked content")
	}
}

func TestMachineProofRejectedRequiresPureCompleteCredentialError(t *testing.T) {
	if !machineProofRejected(controlplane.ErrHelperProof) || !machineProofRejected(fmt.Errorf("wrapped: %w", previewattachment.ErrUnauthorized)) {
		t.Fatal("exact or purely wrapped credential denial was not recognized")
	}
	for name, err := range map[string]error{
		"joined I/O failure": errors.Join(controlplane.ErrHelperProof, syscall.EIO),
		"unknown sibling":    errors.Join(previewattachment.ErrUnauthorized, errors.New("unknown failure")),
	} {
		t.Run(name, func(t *testing.T) {
			if machineProofRejected(err) {
				t.Fatal("mixed error was classified as an expected credential rejection")
			}
		})
	}

	cycle := &machineProofCycleError{}
	cycle.next = errors.Join(controlplane.ErrHelperProof, cycle)
	if machineProofRejected(cycle) {
		t.Fatal("cyclic credential error was treated as a rejection")
	}

	wide := make([]error, maxMachineProofErrorNodes)
	for i := range wide {
		wide[i] = controlplane.ErrHelperProof
	}
	if machineProofRejected(errors.Join(wide...)) {
		t.Fatal("over-bound credential tree was treated as a rejection")
	}

	var typedNil *machineProofCycleError
	if machineProofRejected(typedNil) {
		t.Fatal("typed-nil error was treated as a credential rejection")
	}
}

type machineProofCycleError struct{ next error }

func (*machineProofCycleError) Error() string   { return "cycle" }
func (e *machineProofCycleError) Unwrap() error { return e.next }
