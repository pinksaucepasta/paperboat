package localdaemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestInventoryCompletionOutageRetainsProjectionAndReportsOncePerOwner(t *testing.T) {
	var outage atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/machines":
			_, _ = w.Write([]byte(`{"data":{"items":[{"id":"machine_1","alias":"studio","environment_id":"environment_1"}],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null}}}`))
		case "/v1/previews":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/v1/machines/machine_1/terminal-sessions":
			if outage.Load() {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"PRIVATE"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"items":[],"pagination":{"limit":200,"offset":0,"total":0,"next_offset":null}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx := supportref.WithContext(t.Context(), supportref.New())
	recorder := diagnostics.NewMemoryRecorder()
	source := AuthenticatedMachineSource{ServerURL: server.URL, Auth: &rotatingAuthSource{}, HTTPClient: server.Client()}
	store, err := localapi.NewSnapshotStore(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	inventory, err := NewInventory(InventoryConfig{Source: source, Store: store, Clock: func() time.Time { now = now.Add(time.Second); return now }, OnRefresh: inventoryRefreshObserver(ctx, recorder)})
	if err != nil {
		t.Fatal(err)
	}
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	if err := inventory.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := inventory.Completions(ctx)
	if err != nil || len(original.Items) == 0 {
		t.Fatal("initial completion projection unavailable")
	}
	outage.Store(true)
	for range 2 {
		if err := inventory.Refresh(ctx); err == nil || !errorreport.HTTPAttemptObserved(err) {
			t.Fatal("completion outage swallowed or lost attempt ownership")
		}
		current, err := inventory.Completions(ctx)
		if err != nil || !current.ObservedAt.Equal(original.ObservedAt) {
			t.Fatal("outage replaced last good completion projection")
		}
	}
	if len(faults) != 2 {
		t.Fatalf("HTTP retries duplicated by inventory owner: %d", len(faults))
	}
	outage.Store(false)
	if err := inventory.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := inventory.Completions(ctx)
	if err != nil || !recovered.ObservedAt.After(original.ObservedAt) {
		t.Fatal("successful refresh did not recover completion projection")
	}
	records := recorder.Recent()
	if len(records) != 2 || records[0].Fields["outcome"] != "degraded" || records[1].Fields["outcome"] != "ready" {
		t.Fatalf("retry/recovery records=%+v", records)
	}
	for _, record := range records {
		if record.SupportReference != supportref.FromContext(ctx) {
			t.Fatal("inventory reference changed")
		}
	}
}

type invalidCompletionSource struct{ *scriptedMachineSource }

func (invalidCompletionSource) ListCompletionItems(context.Context, []api.UserMachine) ([]localapi.CompletionItem, error) {
	return []localapi.CompletionItem{{Kind: "machine"}}, nil
}

func TestInventoryCallbackIncludesCompletionValidationFailure(t *testing.T) {
	source := invalidCompletionSource{&scriptedMachineSource{results: []machineResult{{}}}}
	inventory, _ := newTestInventory(t, source, time.Now)
	var reported error
	inventory.onRefresh = func(err error) { reported = err }
	err := inventory.Refresh(t.Context())
	if !errors.Is(err, localapi.ErrInvalidResponse) || !errors.Is(reported, localapi.ErrInvalidResponse) {
		t.Fatal("completion validation failure missed final callback")
	}
}
