package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

func (s *EdgeService) SetNodeCredentials(resolver nodeauth.Resolver) { s.nodeCredentials = resolver }

// Scoped runtime credentials cannot call operations without an exact node
// binding. Account pairing and operator administration have separate handlers.
func (s *EdgeService) authorizeNodeRequest(r *http.Request) (bool, error) {
	if nodeauth.Managed(r, s.credential) {
		return true, nil
	}
	if s.nodeCredentials == nil || nodeauth.Bearer(r) == "" {
		return false, nil
	}
	node, err := s.nodeCredentials.RuntimeNode(r.Context(), nodeauth.Bearer(r))
	if err != nil {
		if nodeauth.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if node == "" {
		return false, nil
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/trust/revocations" {
		return true, nil
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/edge/browser-terminal/control" {
		return r.Header.Get("X-Paperboat-Edge-Node-ID") == node && r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "", nil
	}
	if strings.HasPrefix(r.URL.Path, "/v1/edge/certificates/distributions/") {
		return r.Header.Get("X-Paperboat-Edge-Node-ID") == node, nil
	}
	switch r.URL.Path {
	case "/v1/relay/nodes/start", "/v1/relay/nodes/observe", "/v1/nodes/register", "/v1/nodes/heartbeat", "/v1/edge/routes/desired-state", "/v1/edge/ingress/desired-state", "/v1/edge/inspector/access", "/v1/edge/routes/observations", "/v1/edge/usage-reports":
	default:
		return false, nil
	}
	if r.Method != http.MethodPost || r.Body == nil {
		return false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxEdgeDocument+1))
	closeErr := r.Body.Close()
	if err != nil || closeErr != nil {
		return false, errors.Join(err, closeErr)
	}
	if len(raw) > maxEdgeDocument || canonicaljson.RejectDuplicateFields(raw) != nil {
		return false, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var in struct {
		NodeID     string `json:"node_id"`
		EdgeNodeID string `json:"edge_node_id"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return false, nil
	}
	if in.NodeID != "" && in.NodeID != node || in.EdgeNodeID != "" && in.EdgeNodeID != node {
		return false, nil
	}
	return in.NodeID == node || in.EdgeNodeID == node, nil
}
