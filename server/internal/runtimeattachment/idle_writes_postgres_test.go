package runtimeattachment

import (
	"strings"
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A machine re-attaches every ~25 s with the same request. Before, every attachment rewrote its two carrier
// attachments (a new admission deadline), its edge placement and its latency samples (~1,500 row versions an
// hour per idle machine on staging, PLAN 3b). Now an unchanged attachment returns the stored admission while it
// has more than half its lifetime left, and a similar latency or the same placement is not rewritten; a
// changed sample or an admission past half its lifetime is.
func TestUnchangedAttachmentWritesNoRowsOnPostgres(t *testing.T) {
	f := newAgentFixture(t, "idlewrites")
	xmins := func() string {
		t.Helper()
		var x string
		if err := f.database.SQL().QueryRowContext(f.ctx, `SELECT
 coalesce((SELECT string_agg(edge_node_id||'='||xmin::text, ',' ORDER BY edge_node_id) FROM runtime_carrier_attachments WHERE route_id=$1),'')||' '||
 coalesce((SELECT xmin::text FROM machine_edge_placements WHERE machine_id=$2),'-')||' '||
 coalesce((SELECT string_agg(node_id||'='||xmin::text, ',' ORDER BY node_id) FROM machine_node_latency WHERE machine_id=$2),'-')`, f.route, f.machine).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	request := f.request
	request.EdgePreference = &runtimewire.EdgePreference{PrimaryNodeID: f.node, PrimaryProcessEpoch: f.edge.ProcessEpoch, SecondaryNodeID: f.backupNode, SecondaryProcessEpoch: f.backupEdge.ProcessEpoch}
	request.EdgeLatency = []runtimewire.EdgeLatency{{NodeID: f.node, ProcessEpoch: f.edge.ProcessEpoch, RTTMilliseconds: 40}}
	first, err := f.service.Attach(f.ctx, f.proof, request)
	if err != nil {
		t.Fatal(err)
	}
	// The first attachment learns the candidates; the second one reports against them.
	second, err := f.service.Attach(f.ctx, f.proof, request)
	if err != nil {
		t.Fatal(err)
	}
	stored := xmins()
	if strings.Contains(stored, "-") {
		t.Fatalf("fixture did not store a placement and a latency sample: %s", stored)
	}
	request.EdgeLatency[0].RTTMilliseconds = 42 // within 5 ms: the same measurement
	for range 3 {
		again, err := f.service.Attach(f.ctx, f.proof, request)
		if err != nil {
			t.Fatal(err)
		}
		if !again.Admission.ExpiresAt.Equal(second.Admission.ExpiresAt) || again.Admission.AttachmentGeneration != first.Admission.AttachmentGeneration {
			t.Fatalf("an unchanged attachment changed its admission: %+v -> %+v", second.Admission, again.Admission)
		}
	}
	if after := xmins(); after != stored {
		t.Fatalf("3 unchanged attachments rewrote rows:\n before %s\n after  %s", stored, after)
	}

	request.EdgeLatency[0].RTTMilliseconds = 90
	if _, err := f.service.Attach(f.ctx, f.proof, request); err != nil {
		t.Fatal(err)
	}
	if after := xmins(); after == stored {
		t.Fatal("a changed latency sample was not written")
	}

	// Past half its lifetime the admission is renewed: with a 10 min lifetime the stored 2 min one is.
	longer := &Service{DB: f.database, AdmissionLifetime: 10 * time.Minute}
	renewable := xmins()
	if _, err := longer.Attach(f.ctx, f.proof, request); err != nil {
		t.Fatal(err)
	}
	if after := xmins(); after == renewable {
		t.Fatal("an admission past half its lifetime was not renewed")
	}
}
