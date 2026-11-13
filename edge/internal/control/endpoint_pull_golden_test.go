package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// The server's real full and delta endpoint answers (written by TestEndpointAdmissionDeltasOnPostgres)
// go through this edge's real endpoint client: grouped carriers expand into admissions the edge
// accepts, the delta's removals and expiry arrive, and the request carries the cursor. Only the
// recorded expiry times are moved into the future.
func TestEndpointPullGoldenThroughTheEdgeClient(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/runtimeattachment/testdata/endpoint_pull.json")
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	raw = regexp.MustCompile(`"expires_at": "[^"]+"`).ReplaceAll(raw, []byte(`"expires_at": "`+future+`"`))
	var golden struct {
		Full, Delta json.RawMessage
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	var full runtimewire.Snapshot
	if err := json.Unmarshal(golden.Full, &full); err != nil || full.Carriers == nil || len(*full.Carriers) == 0 {
		t.Fatalf("golden full answer: %v", err)
	}
	carrier := (*full.Carriers)[0].Carrier.Binding
	var sent []runtimewire.EdgeRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request runtimewire.EdgeRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || r.URL.Path != runtimewire.EdgeEndpointPullPath {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		sent = append(sent, request)
		if request.Since == "" {
			_, _ = w.Write(golden.Full)
		} else {
			_, _ = w.Write(golden.Delta)
		}
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	update, err := client.EndpointAdmissions(ctx, carrier.EdgeNodeID, carrier.EdgeProcessEpoch, "")
	if err != nil || !update.Complete || len(update.Admissions) == 0 || update.Cursor == "" {
		t.Fatalf("full answer through the edge client: %+v %v", update, err)
	}
	delta, err := client.EndpointAdmissions(ctx, carrier.EdgeNodeID, carrier.EdgeProcessEpoch, update.Cursor)
	if err != nil || delta.Complete || len(delta.Admissions) != 1 || len(delta.Removed) != 1 || delta.ExpiresAt.IsZero() {
		t.Fatalf("delta answer through the edge client: %+v %v", delta, err)
	}
	if len(sent) != 2 || sent[1].Since != update.Cursor || !runtimewire.HasCapability(sent[0].Capabilities, runtimewire.CapabilityEndpointDeltas) {
		t.Fatalf("requests = %+v", sent)
	}
}
