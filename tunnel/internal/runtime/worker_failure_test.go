package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgehttp"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

func TestWorkerFailuresAreSafeAndRecoveryRetainsAuthority(t *testing.T) {
	const reference = "support_10000000-0000-4000-8000-000000000001"
	if os.Getenv("PB_WORKER_FAILURE_HELPER") == "1" {
		reporter, err := reporting.New("paperboat-tunnel")
		if err != nil {
			t.Fatal(err)
		}
		defer reporter.Close()
		ctx := reporting.WithSupportReference(context.Background(), reference)
		a := runtimeAdmission(t)
		expected, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: a.Binding.EdgeNodeID, ProcessEpoch: a.Binding.EdgeProcessEpoch})
		if err != nil {
			t.Fatal(err)
		}
		routes, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "runtime.example.test", ProcessEpoch: a.Binding.EdgeProcessEpoch})
		if err != nil {
			t.Fatal(err)
		}
		defer routes.Close()
		source := &runtimeSourceFake{admissions: []control.RuntimeCarrierAdmission{a}}
		worker := &RuntimeCarrierWorker{Reporter: reporter, Source: source, Expected: expected, Routes: routes, Timeout: time.Second}
		if err := worker.reconcile(ctx); err != nil || len(expected.Snapshot()) != 1 {
			t.Fatal("initial runtime authority was not installed")
		}
		invalid := a
		invalid.RouteKind = "PRIVATE_PAYLOAD"
		source.admissions = []control.RuntimeCarrierAdmission{invalid}
		if err := worker.reconcile(ctx); err == nil || len(expected.Snapshot()) != 1 {
			t.Fatal("invalid pull replaced authority")
		}
		source.admissions = []control.RuntimeCarrierAdmission{a}
		if err := worker.reconcile(ctx); err != nil || len(expected.Snapshot()) != 1 {
			t.Fatal("runtime recovery failed")
		}
		// An HTTP-owned failure is not repeated at the semantic worker boundary.
		source.err = &control.RequestFailure{Err: control.ErrControlUnavailable, Cause: syscall.ECONNREFUSED}
		if err := worker.reconcile(ctx); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatal("request cause lost")
		}

		now := time.Now().UTC()
		p := testPreviewCarrierAdmission("preview", "operation", "route", "a.preview.example.test", now.Add(time.Hour))
		pe, err := datacarrier.NewExpectedAdmissionRegistry(datacarrier.ExpectedAdmissionRegistryConfig{NodeID: p.Binding.EdgeNodeID, ProcessEpoch: p.Binding.EdgeProcessEpoch})
		if err != nil {
			t.Fatal(err)
		}
		pr, err := edgehttp.NewDataCarrierPreviewRegistry(edgehttp.DataCarrierPreviewRegistryConfig{BaseDomain: "preview.example.test", ProcessEpoch: p.Binding.EdgeProcessEpoch})
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		cause := &os.PathError{Op: "open", Path: "PRIVATE_FILENAME", Err: syscall.EACCES}
		ps := &previewCarrierSourceFake{snapshots: [][]control.PreviewCarrierAdmission{{p}}, ackError: cause}
		pw, err := NewPreviewCarrierWorker(PreviewCarrierWorkerConfig{Reporter: reporter, Source: ps, Expected: pe, Registry: pr, NodeID: p.Binding.EdgeNodeID, ProcessEpoch: p.Binding.EdgeProcessEpoch, Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if err := pw.reconcile(ctx); !errors.Is(err, syscall.EACCES) {
			t.Fatal("preview ACK cause lost")
		}
		for _, a := range pe.Snapshot() {
			if a.Admitted {
				t.Fatal("failed ACK admitted authority")
			}
		}
		ps.ackError = nil
		if err := pw.reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if values := pe.Snapshot(); len(values) != 1 || !values[0].Admitted {
			t.Fatal("preview recovery did not admit authority")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerFailuresAreSafeAndRecoveryRetainsAuthority$")
	command.Env = append(os.Environ(), "PB_WORKER_FAILURE_HELPER=1", "PAPERBOAT_SENTRY_ENABLED=false")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("worker helper failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "PRIVATE_") || strings.Contains(stderr.String(), "a.preview.example.test") {
		t.Fatal("private worker payload leaked")
	}
	var faults []reporting.Fault
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var f reporting.Fault
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal("worker emitted invalid structured diagnostics")
		}
		faults = append(faults, f)
	}
	if len(faults) != 2 {
		t.Fatalf("faults=%d, want exactly two semantic failures", len(faults))
	}
	for i, code := range []string{"runtime_admission_failed", "preview_admission_failed"} {
		if faults[i].Code != code || faults[i].SupportReference != reference || faults[i].CorrelationID != reference || faults[i].SourceFile == "" {
			t.Fatalf("fault %d lost safe contract: %+v", i, faults[i])
		}
	}
	if faults[1].Cause != "permission_denied" || faults[1].Errno != int(syscall.EACCES) {
		t.Fatal("preview cause projection lost")
	}
}
