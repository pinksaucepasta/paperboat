package controlplane

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// A node bearer reaches the peer directory only for the node it is bound to (the body's edge_node_id), as on a heartbeat.
func TestPeersRefusesForeignNodeBearer(t *testing.T) {
	s := &EdgeService{credential: "managed-secret", nodeCredentials: scopedNodeCredential{}}
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"own", `{"edge_node_id":"node_a","process_epoch":"e"}`, true},
		{"foreign", `{"edge_node_id":"node_b","process_epoch":"e"}`, false},
		{"no identity", `{"process_epoch":"e"}`, false},
		{"duplicate identity", `{"edge_node_id":"node_b","edge_node_id":"node_a"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", nodemesh.PeersPath, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer node-secret")
			if got, err := s.authorizeNodeRequest(r); err != nil || got != tc.want {
				t.Fatalf("authorized=%v err=%v want %v", got, err, tc.want)
			}
		})
	}
	// Through the handler a foreign body never reaches the directory (no store is needed to refuse it).
	r := httptest.NewRequest("POST", nodemesh.PeersPath, strings.NewReader(`{"edge_node_id":"node_b","process_epoch":"e"}`))
	r.Header.Set("Authorization", "Bearer node-secret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("foreign node bearer got %d, want 401", w.Code)
	}
}
