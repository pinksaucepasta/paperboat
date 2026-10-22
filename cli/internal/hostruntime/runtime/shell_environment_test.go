//go:build darwin || linux || windows

package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
)

type fakeOwnerShellEnvironment struct {
	calls  int
	values []string
	err    error
}

func (f *fakeOwnerShellEnvironment) OwnerShellEnvironment(_ context.Context, workspace, account string) ([]string, error) {
	f.calls++
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
