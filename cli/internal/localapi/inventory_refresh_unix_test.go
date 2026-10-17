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
