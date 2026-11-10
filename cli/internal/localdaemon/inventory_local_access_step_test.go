package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type stepFailure struct{ step string }

func (e stepFailure) Error() string           { return "local access step failed" }
func (e stepFailure) LocalAccessStep() string { return e.step }

// stepCause is a step failure that wraps its cause, like the coordinator's operation errors.
type stepCause struct {
	step  string
	cause error
}

func (e stepCause) Error() string           { return "local access step failed" }
func (e stepCause) Unwrap() error           { return e.cause }
func (e stepCause) LocalAccessStep() string { return e.step }

// pb doctor names the local access step that fails: a step still being retried while native access works keeps
// the daemon ready with a warning, and a failed reconciliation names its step in the error title.
func TestInventoryNamesTheFailingLocalAccessStep(t *testing.T) {
	for _, c := range []struct {
		name, state, code, severity, step string
		err                               error
	}{
		{"pending", "ready", "browser_urls_unavailable", "warning", "browser certificate", LocalAccessPendingError{Step: "browser_certificate"}},
		{"pending port", "ready", "browser_urls_unavailable", "warning", "browser port listener", LocalAccessPendingError{Step: "browser_port_listener"}},
		{"busy gateway port", "ready", "browser_urls_unavailable", "warning", "another program is using port 80/443", LocalAccessPendingError{Step: "browser_gateway_port", Detail: "nginx, PID 4242"}},
		{"failed", "degraded", "local_access_unavailable", "error", "machine names", stepFailure{step: "machine_names"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
			inventory, err := NewInventory(InventoryConfig{
				Source:            &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
				Store:             store,
				RefreshInterval:   time.Second,
				RequestTimeout:    time.Second,
				Clock:             func() time.Time { return now },
				ReconcileMachines: func(context.Context, []api.UserMachine) error { return c.err },
			})
			if err != nil {
				t.Fatal(err)
			}
			inventory.setupSince = time.Unix(1, 0) // past start-up: a failure is degraded
			refreshErr := inventory.Refresh(context.Background())
			if c.state == "ready" && refreshErr != nil {
				t.Fatalf("a pending step must not fail the refresh: %v", refreshErr)
			}
			snapshot, err := store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.DaemonState != c.state || len(snapshot.Health) != 1 {
				t.Fatalf("snapshot state=%q health=%#v", snapshot.DaemonState, snapshot.Health)
			}
			health := snapshot.Health[0]
			if health.Code != c.code || health.Severity != c.severity || !strings.Contains(health.Title, c.step) {
				t.Fatalf("health=%#v; want %s/%s naming %q", health, c.code, c.severity, c.step)
			}
			if c.name == "busy gateway port" && (!strings.Contains(health.Title, "nginx, PID 4242") || !strings.Contains(health.Recovery, "Stop that program, and Paperboat turns browser URLs back on by itself")) {
				t.Fatalf("health=%#v; want the owner in the title and the plain recovery", health)
			}
		})
	}
	if errors.Is(LocalAccessPendingError{Step: "x"}, context.Canceled) {
		t.Fatal("unexpected")
	}
}

// pbvm2, 2026-11-09: a server 401 or a signed-out credential during the reconcile said "repair the installation".
// It is the signed-out state; a 403 and local failures stay local access failures.
func TestInventoryReconcileSignedOutIsNotALocalAccessFailure(t *testing.T) {
	for name, c := range map[string]struct {
		err         error
		code, state string
	}{
		"server 401":      {fmt.Errorf("refresh authorized local browser services: %w", &api.APIError{Status: 401, Code: "unauthorized"}), "authentication_required", "awaiting_enrollment"},
		"no credentials":  {fmt.Errorf("refresh local machine access credentials: %w", config.ErrNoCredentials), "authentication_required", "awaiting_enrollment"},
		"server 403":      {&api.APIError{Status: 403, Code: "forbidden"}, "local_access_unavailable", "degraded"},
		"guard unreached": {stepFailure{step: "guard_connect"}, "local_access_unavailable", "degraded"},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := NewInventory(InventoryConfig{
				Source:            &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}},
				Store:             store,
				RefreshInterval:   time.Second,
				RequestTimeout:    time.Second,
				ReconcileMachines: func(context.Context, []api.UserMachine) error { return c.err },
			})
			if err != nil {
				t.Fatal(err)
			}
			inventory.setupSince = time.Unix(1, 0)
			_ = inventory.Refresh(context.Background())
			snapshot, err := store.Snapshot(context.Background())
			if err != nil || snapshot.DaemonState != c.state || len(snapshot.Health) != 1 || snapshot.Health[0].Code != c.code {
				t.Fatalf("snapshot=%#v err=%v, want %s with %s", snapshot, err, c.state, c.code)
			}
		})
	}
}

// The health title and the journal name the failing step and the error's class, never its text.
func TestLocalAccessCauseNamesStepAndClassWithoutText(t *testing.T) {
	secret := "token=SECRET-VALUE"
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"step with server refusal": {fmt.Errorf("%s: %w", secret, stepCause{step: "browser_services", cause: &api.APIError{Status: 403, Code: "forbidden"}}), "the browser services step failed (permission_denied, HTTP 403)"},
		"no step":                  {errors.New(secret), "a local access step failed (internal, "},
	} {
		t.Run(name, func(t *testing.T) {
			got := LocalAccessCause(c.err)
			if !strings.HasPrefix(got, c.want) || strings.Contains(got, "SECRET") {
				t.Fatalf("cause=%q, want prefix %q and no error text", got, c.want)
			}
		})
	}
}
