//go:build darwin || linux

package localapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type refreshingSnapshotSource struct{ calls int }

func (*refreshingSnapshotSource) Snapshot(context.Context) (Snapshot, error) {
	return validSnapshot(), nil
}
func (s *refreshingSnapshotSource) Refresh(context.Context) error { s.calls++; return nil }

func TestInventoryRefreshRequiresOwnerAndEmptyMutation(t *testing.T) {
	source := &refreshingSnapshotSource{}
	server, err := NewServer(ServerConfig{SocketPath: filepath.Join(localAPITestDir(t), "api.sock"), OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: source, Authorize: func(Peer) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, body string
		uid, status        int
	}{
		{"read-authorized non-owner", http.MethodPost, "", os.Geteuid() + 1, http.StatusForbidden},
		{"read is not mutation", http.MethodGet, "", os.Geteuid(), http.StatusBadRequest},
		{"payload rejected", http.MethodPost, "{}", os.Geteuid(), http.StatusBadRequest},
		{"owner refresh", http.MethodPost, "", os.Geteuid(), http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := source.calls
			request := httptest.NewRequest(tc.method, "/v1/inventory/refresh", strings.NewReader(tc.body))
			request = request.WithContext(context.WithValue(request.Context(), peerContextKey{}, Peer{UID: tc.uid}))
			response := httptest.NewRecorder()
			server.handler().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d", response.Code, tc.status)
			}
			want := before
			if tc.status == http.StatusNoContent {
				want++
			}
			if source.calls != want {
				t.Fatalf("unauthorized or malformed request changed inventory: calls=%d want=%d", source.calls, want)
			}
		})
	}
}

type deadlineSource struct {
	refreshingSnapshotSource
	budget time.Duration
}

func (s *deadlineSource) Refresh(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.DeadlineExceeded
	}
	s.budget = time.Until(deadline)
	return nil
}

// A refresh the caller asked for gets a budget for all its stages, not the ordinary request timeout:
// a sign-in on a large account failed because the refresh was cut off after 5 seconds.
func TestInventoryRefreshHasItsOwnLongTimeout(t *testing.T) {
	source := &deadlineSource{}
	server, err := NewServer(ServerConfig{SocketPath: filepath.Join(localAPITestDir(t), "api.sock"), OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: source, Authorize: func(Peer) bool { return true }, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/inventory/refresh", nil)
	request = request.WithContext(context.WithValue(request.Context(), peerContextKey{}, Peer{UID: os.Geteuid()}))
	response := httptest.NewRecorder()
	server.handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || source.budget < 30*time.Second {
		t.Fatalf("status=%d budget=%v, want 204 and at least 30s", response.Code, source.budget)
	}
}
