package selfhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestSelfhostOperationalSQLFailuresKeepCause(t *testing.T) {
	store := openClosedPostgres(t)
	service := New(store, strings.Repeat("k", 32))
	tests := []struct {
		name       string
		call       func() error
		misclassed error
	}{
		{
			name:       "runtime node lookup",
			call:       func() error { _, err := service.RuntimeNode(context.Background(), strings.Repeat("c", 32)); return err },
			misclassed: nodeauth.ErrNotFound,
		},
		{
			name:       "runtime credential lookup",
			call:       func() error { _, err := service.RuntimeCredential(context.Background(), "node_fault_01"); return err },
			misclassed: nodeauth.ErrNotFound,
		},
		{
			name: "placement owner lookup",
			call: func() error {
				_, err := service.SetPlacement(context.Background(), "account_fault_01", "preview", "preview_fault_01", nil)
				return err
			},
			misclassed: ErrUnauthorized,
		},
		{
			name: "team scope lookup",
			call: func() error {
				_, err := service.ListScoped(context.Background(), "account_fault_01", Scope{Kind: "team", ID: "team_fault_01"}, false)
				return err
			},
			misclassed: ErrUnauthorized,
		},
		{
			name: "ingress authorization lookup",
			call: func() error {
				return service.VerifyIngressScoped(context.Background(), "account_fault_01", "installation_fault_01", false)
			},
			misclassed: ErrUnauthorized,
		},
		{
			name:       "ingress node lookup",
			call:       func() error { return service.VerifyIngress(context.Background(), "installation_fault_01") },
			misclassed: ErrIngress,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("connection refusal was not retained (error type %T)", err)
			}
			if errors.Is(err, test.misclassed) {
				t.Fatalf("operational failure was misclassified as %v", test.misclassed)
			}
		})
	}
}

func TestIngressReconcilerReportsFailureOnceAndRecovery(t *testing.T) {
	store := openClosedPostgres(t)
	service := New(store, strings.Repeat("k", 32))
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for range 2 {
		err := service.ReconcileIngress(context.Background())
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("reconciliation did not preserve the database failure (error type %T)", err)
		}
	}
	service.observeIngressIteration(context.Background(), nil)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("repeated failure and recovery produced %d records, want 2", len(lines))
	}
	var failed, recovered map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &failed); err != nil {
		t.Fatalf("decode failure record: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &recovered); err != nil {
		t.Fatalf("decode recovery record: %v", err)
	}
	if failed["stage"] != "reconciliation" || failed["code"] != "edge_dns_reconcile_failed" || failed["cause"] != "connection_refused" || failed["errno"] != float64(syscall.ECONNREFUSED) {
		t.Fatalf("failure record lost bounded cause metadata: %s", lines[0])
	}
	reference, ok := failed["support_reference"].(string)
	if !ok || !supportreport.ValidReference(reference) {
		t.Fatalf("failure record omitted a valid support reference: %s", lines[0])
	}
	if recovered["code"] != "worker_recovered" || recovered["outcome"] != "success" || recovered["support_reference"] != reference {
		t.Fatalf("recovery record did not close the same incident: %s", lines[1])
	}
	if strings.Contains(output.String(), "SELFHOST_TEST_PASSWORD_CANARY") {
		t.Fatal("diagnostic record exposed the test database password")
	}
}

func TestIngressReconcilerReportsBoundIncidentAndRecovery(t *testing.T) {
	records := make(chan supportreport.Record, 4)
	reporter, err := supportreport.New(selfhostObservationStore{records: records}, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reporter.Run(ctx)
	}()

	service := &Service{}
	bound := reporter.Context(ctx, "")
	service.observeIngressIteration(bound, syscall.ECONNREFUSED)
	service.observeIngressIteration(bound, syscall.ECONNREFUSED)
	service.observeIngressIteration(bound, nil)
	cancel()
	<-done

	var failure, recovery supportreport.Record
	for i := 0; i < 2; i++ {
		select {
		case record := <-records:
			if record.Code == "edge_dns_reconcile_failed" {
				failure = record
			} else if record.Code == "worker_recovered" {
				recovery = record
			}
		default:
			t.Fatal("bound reporter did not persist both incident records")
		}
	}
	if failure.Cause != "connection_refused" || !supportreport.ValidReference(failure.Reference) {
		t.Fatalf("bound failure record lost its cause or reference: %+v", failure)
	}
	if recovery.Code != "worker_recovered" || recovery.Reference != failure.Reference {
		t.Fatalf("bound recovery did not close the same incident: failure=%+v recovery=%+v", failure, recovery)
	}
}

type selfhostObservationStore struct{ records chan supportreport.Record }

func (s selfhostObservationStore) Save(_ context.Context, record supportreport.Record) error {
	s.records <- record
	return nil
}
func (selfhostObservationStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (selfhostObservationStore) Cleanup(context.Context) error { return nil }

func TestIngressProbeFailureKeepsStaticProseAndCause(t *testing.T) {
	private := errors.New("INGRESS_PRIVATE_RESPONSE_CANARY")
	err := ingressProbeError(errors.Join(private, syscall.ECONNRESET))
	if !IsIngressProbeFailure(err) || !errors.Is(err, ErrIngress) || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatal("probe failure lost its typed retry outcome or underlying cause")
	}
	if strings.Contains(err.Error(), private.Error()) {
		t.Fatal("probe failure exposed arbitrary provider response text")
	}
	if IsIngressProbeFailure(errors.Join(err, syscall.EIO)) {
		t.Fatal("joined independent failure was accepted as a pure ingress probe outcome")
	}
	if IsIngressProbeFailure(errors.Join(ErrIngress, syscall.EIO)) {
		t.Fatal("joined expected and operational failures were accepted as a pure ingress probe outcome")
	}
	var typedNil *ingressProbeFailure
	if IsIngressProbeFailure(typedNil) {
		t.Fatal("typed-nil ingress failure was accepted as an expected outcome")
	}
}

func TestIngressReconcilerKeepsOperationalCauseWithCancellation(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service := &Service{}
	service.observeIngressIteration(context.Background(), errors.Join(context.Canceled, syscall.EIO))
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatalf("decode bounded mixed-cause event: %v", err)
	}
	if event["cause"] != "system_error" || event["errno"] != float64(syscall.EIO) || event["outcome"] != "failed" {
		t.Fatalf("substantive cause was masked by cancellation: %s", output.String())
	}
}

func openClosedPostgres(t *testing.T) *db.DB {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("reserve a closed PostgreSQL test port")
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal("close the reserved PostgreSQL test port")
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: "postgres://selfhost_test:SELFHOST_TEST_PASSWORD_CANARY@" + address + "/selfhost_test?sslmode=disable&connect_timeout=1"})
	if err != nil {
		t.Fatalf("open lazy SQL store (error type %T)", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
