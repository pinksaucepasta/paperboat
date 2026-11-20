package runtimeattachment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"

	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

// TestHoldersRequestIgnoresUnknownFields: a holders request is a receiver of an edge's request, so an unknown field is
// ignored (AGENTS.md: clients ignore unknown fields), while a wrong-typed known field or a missing edge_node_id is refused.
func TestHoldersRequestIgnoresUnknownFields(t *testing.T) {
	f := newAgentFixture(t, "holdreq")
	verifier := edgeVerifierFunc(func(context.Context, *http.Request, []byte) (previewattachment.PreviewEdgeIdentity, error) {
		return f.edge, nil
	})
	h := &Handler{Service: f.service, Edge: verifier}
	serve := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.serveHolders(rec, httptest.NewRequest(http.MethodPost, "/v1/edge/forward/holders", nil), []byte(body))
		return rec
	}

	accepted := serve(fmt.Sprintf(`{"edge_node_id":%q,"process_epoch":%q,"added_later":{"nested":[1,2]}}`, f.node, f.edge.ProcessEpoch))
	if accepted.Code != http.StatusOK {
		t.Fatalf("unknown field: status %d, want 200; body %s", accepted.Code, accepted.Body.String())
	}
	var answer nodemesh.HoldersResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &answer); err != nil {
		t.Fatalf("unknown field: answer is not a holders response: %v", err)
	}

	refused := map[string]string{
		"wrong-typed limit":    fmt.Sprintf(`{"edge_node_id":%q,"process_epoch":%q,"limit":"ten"}`, f.node, f.edge.ProcessEpoch),
		"missing edge_node_id": fmt.Sprintf(`{"process_epoch":%q}`, f.edge.ProcessEpoch),
	}
	for name, body := range refused {
		if rec := serve(body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %s", name, rec.Code, rec.Body.String())
		}
	}
}
