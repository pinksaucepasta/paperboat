package usermachines

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

func TestEnrollmentStartedAuditFailureIsReported(t *testing.T) {
	store, err := db.Open(config.Database{
		Driver: "postgres",
		DSN:    "postgres://audit_test:SECRET_AUDIT_PASSWORD@127.0.0.1:5432/audit_test?sslmode=disable",
	})
	if err != nil {
		t.Fatal("open lazy test pool")
	}
	if err := store.Close(); err != nil {
		t.Fatal("close test pool")
	}

	var output strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ref := supportreport.NewReference()
	const correlation = "request_enrollment_audit_42"
	ctx := serverdiagnostics.WithCorrelationID(context.Background(), correlation)
	ctx = supportreport.WithFailureIdentity(ctx, "private_owner", correlation, ref)
	event := audit.Event{
		ActorUserID:    "private_owner",
		ActorType:      audit.ActorUser,
		EventType:      "user_machine.enrollment_started",
		ResourceType:   "pairing",
		ResourceID:     "SECRET_PAIRING_ID",
		IdempotencyKey: "SECRET_IDEMPOTENCY_KEY",
		Metadata:       map[string]any{"private": "SECRET_AUDIT_METADATA"},
	}
	err = recordEnrollmentStartedAudit(ctx, audit.NewWriter(store), event)
	if err == nil {
		t.Fatal("closed audit store unexpectedly accepted the event")
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(output.String()), &record); err != nil {
		t.Fatalf("expected one structured diagnostic record: %v", err)
	}
	for key, want := range map[string]string{
		"component":         "paperboat-server",
		"operation":         "http_machines",
		"stage":             "service_error",
		"code":              "user_machine_enrollment_audit_failed",
		"support_reference": ref,
		"correlation_id":    correlation,
		"outcome":           "failed",
		"severity":          "error",
	} {
		if got, _ := record[key].(string); got != want {
			t.Errorf("diagnostic %s = %q, want %q", key, got, want)
		}
	}
	if got, _ := record["cause"].(string); got == "" || got == "none" {
		t.Errorf("diagnostic omitted the audit-store failure cause: %q", got)
	}
	if got, _ := record["error_type"].(string); got == "" || got == "none" {
		t.Errorf("diagnostic omitted the bounded audit-store error type: %q", got)
	}
	logged := output.String()
	if strings.Contains(logged, err.Error()) {
		t.Fatal("diagnostic included the raw audit-store error")
	}
	for _, private := range []string{"SECRET_AUDIT_PASSWORD", "SECRET_PAIRING_ID", "SECRET_IDEMPOTENCY_KEY", "SECRET_AUDIT_METADATA", "private_owner"} {
		if strings.Contains(logged, private) {
			t.Errorf("diagnostic leaked %s", private)
		}
	}
}
