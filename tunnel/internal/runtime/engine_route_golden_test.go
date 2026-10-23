package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
)

// The engine route row the server sends (written by the server's real snapshot code in
// TestTunnelsMoveOntoTheEngineOnlyWhenAgentAndEdgesServeThemOnPostgres) goes through the edge's real
// route decoder and durable admission conversion into the real admission registry, which names the
// agent's runtime carrier as its replica. Round 21 shipped a thumbprint format the registry refused,
// so no engine route was ever ready; each side had tested its own hand-built shape.
func TestEdgeAdmitsTheServersEngineRouteGolden(t *testing.T) {
	golden, err := os.ReadFile("../../../shared/runtimeattachment/testdata/engine_route_snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/edge/routes/desired-state" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(golden)
	}))
	defer server.Close()
	client, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: server.URL, Client: server.Client(), Credential: strings.Repeat("x", 32), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// The node and epoch the golden row is fenced to.
	var probe struct{ node, epoch string }
	for _, field := range []struct {
		name string
		into *string
	}{{"edge_node_id", &probe.node}, {"edge_process_epoch", &probe.epoch}} {
		marker := `"` + field.name + `": "`
		start := strings.Index(string(golden), marker)
		if start < 0 {
			t.Fatalf("golden lacks %s", field.name)
		}
		rest := string(golden)[start+len(marker):]
		*field.into = rest[:strings.Index(rest, `"`)]
	}
	snapshot, err := client.DesiredRouteSnapshot(context.Background(), probe.node, probe.epoch)
	if err != nil || !snapshot.Canonical || len(snapshot.Routes) != 1 {
		t.Fatalf("edge decoder refused the server's engine route: %+v %v", snapshot, err)
	}
	admissions, err := canonicalDurableAdmissions(snapshot.Routes)
	if err != nil || len(admissions) != 1 {
		t.Fatalf("engine route did not convert to a durable admission: %v", err)
	}
	registry, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: probe.node, ProcessEpoch: probe.epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	now := time.Now().UTC()
	if err := registry.Replace(admissions, now); err != nil {
		t.Fatalf("the edge's admission registry refused the server's engine route: %v", err)
	}
	if !registry.HasIdentity(admissions[0].Identity, now) {
		t.Fatal("the agent's runtime carrier identity is not a replica of the engine route")
	}
	if key, thumbprint, ok := registry.MachineBinding(admissions[0].Identity, now); !ok || key == "" || thumbprint == "" {
		t.Fatal("no machine binding for the engine route's replica")
	}
}
