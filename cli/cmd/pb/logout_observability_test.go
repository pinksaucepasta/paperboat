package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLogoutFailedRevocationIsUnconfirmedThenFreshSessionCompletes(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"PRIVATE_CODE","message":"PRIVATE_RESPONSE"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{}}`)
	}))
	defer server.Close()
	restore := errorreport.Install(nil)
	defer restore()
	reference := supportref.New()
	var mu sync.Mutex
	var faults []errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { mu.Lock(); faults = append(faults, f); mu.Unlock() })
	defer restoreObserver()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	writeTestProfile(t, dir, cfgPath, server.URL)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(expected string) {
		t.Helper()
		set := flag.NewFlagSet("logout", flag.ContinueOnError)
		set.Bool("json", true, "")
		action := command.NewContext(set)
		action.Context = supportref.WithContext(t.Context(), reference)
		var stdout, stderr bytes.Buffer
		action.Writer = &stdout
		action.ErrWriter = &stderr
		if err := authLogoutSessions(action, cfg, store, nil); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			OK   bool `json:"ok"`
			Data struct {
				SignedOut  bool   `json:"signed_out"`
				Revocation string `json:"server_revocation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || !envelope.Data.SignedOut || envelope.Data.Revocation != expected {
			t.Fatalf("logout result=%q", stdout.String())
		}
		if stderr.Len() != 0 || strings.Contains(stdout.String(), "PRIVATE") {
			t.Fatal("private response or warning contaminated machine output")
		}
		if _, err := store.Load(server.URL); !errors.Is(err, config.ErrNoCredentials) {
			t.Fatal("local credentials remain")
		}
		if records, err := store.PendingRevocations(server.URL); err != nil || len(records) != 0 {
			t.Fatal("logout retained revocation credentials")
		}
	}
	invoke("unconfirmed")
	mu.Lock()
	count := len(faults)
	hasStatus := false
	for _, fault := range faults {
		if fault.SupportReference == reference && fault.HTTPStatus == 503 {
			hasStatus = true
		}
	}
	mu.Unlock()
	if count == 0 || !hasStatus {
		t.Fatal("actual HTTP503 lost status/reference diagnostics")
	}
	fail.Store(false)
	writeTestProfile(t, dir, cfgPath, server.URL)
	invoke("complete")
	if requests.Load() != 2 {
		t.Fatalf("actual revocation attempts=%d", requests.Load())
	}
}

func TestLogoutDeadlineReturnsAfterRequestStopsAndDoesNotClaimReceipt(t *testing.T) {
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(stopped) }))
	defer server.Close()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	writeTestProfile(t, dir, cfgPath, server.URL)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	set := flag.NewFlagSet("logout", flag.ContinueOnError)
	set.Bool("json", true, "")
	action := command.NewContext(set)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	action.Context = ctx
	var out bytes.Buffer
	action.Writer = &out
	action.ErrWriter = io.Discard
	if err := authLogoutSessions(action, cfg, store, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"server_revocation":"unconfirmed"`) {
		t.Fatal("deadline falsely claimed completed revocation")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("revocation request outlived canceled owner")
	}
}

func TestPreferenceFailureAndInteractiveCancellationKeepMixedCauses(t *testing.T) {
	cause := errors.Join(context.Canceled, io.ErrUnexpectedEOF)
	failure := preferenceLoadFailure{cause: cause}
	if !errors.Is(failure, io.ErrUnexpectedEOF) || classifyCommandFailure(failure).kind != commandUnexpected || !strings.Contains(userFacingError(failure), "--no-customization") {
		t.Fatal("preferences failure was mistaken for invocation/cancellation")
	}
	if interactiveCanceled(errors.Join(selector.ErrCanceled, io.ErrUnexpectedEOF)) {
		t.Fatal("interactive cancellation hid operational cause")
	}
	if !interactiveCanceled(selector.ErrCanceled) {
		t.Fatal("pure interactive cancellation changed")
	}
}
