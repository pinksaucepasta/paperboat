//go:build darwin || linux || windows

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
)

type fakeOwnerShellEnvironment struct {
	mu     sync.Mutex
	block  chan struct{}
	calls  int
	values []string
	names  []EnvironmentName
	err    error
}

func (f *fakeOwnerShellEnvironment) OwnerEnvironmentNames(_ context.Context, workspace, account string) ([]EnvironmentName, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if account != "acct_owner" {
		return nil, errShellEnvironmentAccount
	}
	return f.names, f.err
}

func (f *fakeOwnerShellEnvironment) OwnerShellEnvironment(_ context.Context, workspace, account string) ([]string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if account != "acct_owner" {
		return nil, errShellEnvironmentAccount
	}
	return f.values, f.err
}

func serveShellEnvironment(handler http.Handler, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, ShellEnvironmentPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestShellEnvironmentRequiresTheLocalControlToken(t *testing.T) {
	source := &fakeOwnerShellEnvironment{values: []string{"A=1"}}
	handler := newShellEnvironmentHandler(source, "secret")
	if got := serveShellEnvironment(handler, "wrong", `{"workspace_id":"personal","account_id":"acct_owner"}`); got.Code != http.StatusUnauthorized || source.calls != 0 {
		t.Fatalf("wrong token: %d calls=%d", got.Code, source.calls)
	}
	if newShellEnvironmentHandler(source, "") != nil {
		t.Fatal("handler without a control token must not be mounted")
	}
}

func TestShellEnvironmentServesOnlyTheOwnerAndCachesInMemory(t *testing.T) {
	source := &fakeOwnerShellEnvironment{values: []string{"A=1"}}
	handler := newShellEnvironmentHandler(source, "secret").(*shellEnvironmentHandler)
	now := time.Unix(1000, 0)
	handler.now = func() time.Time { return now }
	if got := serveShellEnvironment(handler, "secret", `{"workspace_id":"personal","account_id":"acct_other"}`); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "account_mismatch") {
		t.Fatalf("other account: %d %s", got.Code, got.Body.String())
	}
	for range 2 {
		got := serveShellEnvironment(handler, "secret", `{"workspace_id":"personal","account_id":"acct_owner"}`)
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"A=1"`) {
			t.Fatalf("owner: %d %s", got.Code, got.Body.String())
		}
	}
	if source.calls != 2 {
		t.Fatalf("second owner request must be served from memory, calls=%d", source.calls)
	}
	now = now.Add(shellEnvironmentTTL + time.Second)
	serveShellEnvironment(handler, "secret", `{"workspace_id":"personal","account_id":"acct_owner"}`)
	if source.calls != 3 {
		t.Fatalf("expired cache must refresh, calls=%d", source.calls)
	}
}

func TestShellEnvironmentReportsPendingAndRejectsBadBodies(t *testing.T) {
	source := &fakeOwnerShellEnvironment{err: &envinject.DeliveryError{Cause: envinject.ErrNotReady}}
	handler := newShellEnvironmentHandler(source, "secret")
	got := serveShellEnvironment(handler, "secret", `{"workspace_id":"personal","account_id":"acct_owner"}`)
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), envinject.LaunchNoticePending) {
		t.Fatalf("pending: %d %s", got.Code, got.Body.String())
	}
	for _, body := range []string{`{}`, `{"workspace_id":"personal","account_id":"acct_owner","x":1}`, `not json`} {
		if got := serveShellEnvironment(handler, "secret", body); got.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, got.Code)
		}
	}
}

func TestShellEnvironmentFetchOutlivesAnImpatientCallerAndCachesFailure(t *testing.T) {
	source := &fakeOwnerShellEnvironment{block: make(chan struct{}), err: &envinject.DeliveryError{Cause: envinject.ErrNotReady}}
	handler := newShellEnvironmentHandler(source, "secret").(*shellEnvironmentHandler)
	in := shellEnvironmentRequest{WorkspaceID: "personal", AccountID: "acct_owner"}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := handler.environment(ctx, in); !envinject.Undeliverable(err) {
		t.Fatalf("impatient caller: %v", err)
	}
	close(source.block)
	<-handler.entry(in).done
	started := time.Now()
	if _, err := handler.environment(t.Context(), in); err == nil || time.Since(started) > 10*time.Millisecond {
		t.Fatalf("cached failure: %v after %v", err, time.Since(started))
	}
	if source.calls != 1 {
		t.Fatalf("one fetch expected, got %d", source.calls)
	}
}

func serveShellEnvironmentNames(handler http.Handler, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, ShellEnvironmentNamesPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// The names endpoint answers names and layers only, needs the control token, is the owner's alone, is
// refused with its own code when the workspace turned listing off, and is never served from the value cache.
func TestShellEnvironmentNamesNeverCarryValuesAndHonourTheSetting(t *testing.T) {
	const body = `{"workspace_id":"personal","account_id":"acct_owner"}`
	source := &fakeOwnerShellEnvironment{values: []string{"STRIPE_API_KEY=sk-secret"}, names: []EnvironmentName{{Name: "STRIPE_API_KEY", Scope: "personal"}}}
	handler := newShellEnvironmentHandler(source, "secret")
	if got := serveShellEnvironmentNames(handler, "wrong", body); got.Code != http.StatusUnauthorized || source.calls != 0 {
		t.Fatalf("wrong token: %d calls=%d", got.Code, source.calls)
	}
	if got := serveShellEnvironmentNames(handler, "secret", `{"workspace_id":"personal","account_id":"acct_other"}`); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "account_mismatch") {
		t.Fatalf("other account: %d %s", got.Code, got.Body.String())
	}
	for range 2 {
		got := serveShellEnvironmentNames(handler, "secret", body)
		if got.Code != http.StatusOK || strings.TrimSpace(got.Body.String()) != `{"data":{"names":[{"name":"STRIPE_API_KEY","scope":"personal"}]}}` || strings.Contains(got.Body.String(), "sk-secret") {
			t.Fatalf("names: %d %s", got.Code, got.Body.String())
		}
	}
	if source.calls != 3 {
		t.Fatalf("names must not be cached, so turning the setting off applies at once: calls=%d", source.calls)
	}
	source.err = errEnvironmentNamesDisabled
	if got := serveShellEnvironmentNames(handler, "secret", body); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "names_listing_disabled") {
		t.Fatalf("setting off: %d %s", got.Code, got.Body.String())
	}
	source.err = &envinject.DeliveryError{Cause: envinject.ErrNotReady}
	if got := serveShellEnvironmentNames(handler, "secret", body); got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), envinject.LaunchNoticePending) {
		t.Fatalf("pending: %d %s", got.Code, got.Body.String())
	}
}
