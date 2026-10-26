package testutil

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// EnginePreview serves an existing preview lease on its owner machine's runtime carrier to an edge:
// the agent's carrier, the preview endpoint, its route at RouteRevision and an edge assignment at
// AssignmentGeneration that the edge observed ready, so preview_routes lists the preview ready.
type EnginePreview struct {
	Suffix, Lease, Account, Machine, Node, Epoch, PublicKey string
	RouteRevision, WorkerGeneration, ConfigGeneration       int64
	AssignmentGeneration                                    int64
	AccessMode                                              string // default "team"
	Now                                                     time.Time
}

// Serve inserts the rows through exec. The caller deletes the machine's control environment at cleanup, which
// removes the carrier route (and with it the carrier).
func (p EnginePreview) Serve(t *testing.T, exec func(string, ...any)) {
	t.Helper()
	or := func(value, fallback int64) int64 {
		if value == 0 {
			return fallback
		}
		return value
	}
	mode := p.AccessMode
	if mode == "" {
		mode = "team"
	}
	carrierRoute := "runtime_" + p.Suffix
	session := fmt.Sprintf("session_00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)
	admission := fmt.Sprintf(`{"binding":{"tunnel_id":"pvc-tun-%s","connector_id":"pvc-con-%s","config_generation":%d,"edge_carrier_server_spki_sha256":"sha256:%s","edge_carrier_server_certificate_chain_pem":"test-public-certificate-chain"},"config_content_hash":"sha256:%s","edge_endpoints":["h2://edge.example.test:25001","h3://edge.example.test:25002"]}`, p.Suffix, p.Suffix, or(p.ConfigGeneration, 1), strings.Repeat("b", 64), strings.Repeat("a", 64))
	exec(`UPDATE paperboat.preview_leases SET ready_at=$2 WHERE id=$1`, p.Lease, p.Now)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, p.Machine)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1,environment_id,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, carrierRoute, p.Machine)
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) VALUES($1,$2,$3,1,$4,'process_engine',1,$5,$6,$7,1,$8,$8,$9::jsonb,$10,true)`, carrierRoute, p.Machine, p.Account, or(p.WorkerGeneration, 1), p.Node, p.Epoch, p.PublicKey, session, admission, p.Now.Add(20*time.Minute))
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,preview_lease_id) VALUES($1,'preview',$2,$3,$4,'lease',$1)`, p.Lease, p.Account, p.Machine, mode)
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,revision) VALUES($1,$1,$2,'http',$3)`, p.Lease, "pv-"+strings.ReplaceAll(p.Suffix, "_", "-")+".preview.example.test", or(p.RouteRevision, 1))
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,generation,observed_ready,observed_at) VALUES($1,$1,$2,$3,$4,$5,true,$6)`, p.Lease, p.Node, p.Epoch, session, or(p.AssignmentGeneration, 1), p.Now)
}
