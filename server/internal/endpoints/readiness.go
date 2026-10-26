package endpoints

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// ErrNotServing means no edge has observed the preview's endpoint route yet.
var ErrNotServing = errors.New("preview endpoint route is not served by any edge yet")

// PreviewReadiness gates a preview's readiness report: it is ready once its agent serves endpoints
// on an edge that admits them and at least one edge serves its route (preview_routes).
type PreviewReadiness struct {
	DB *db.DB
}

func (r PreviewReadiness) RequirePreviewAttachmentReady(ctx context.Context, accountID, previewID, operationID, ownerMachineID, ownerSessionID string) error {
	var engine, serving bool
	err := r.DB.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM endpoints e JOIN runtime_carrier_attachments a ON a.machine_id=e.agent_machine_id
 AND a.agent_capabilities @> ARRAY['endpoints']::text[]
 JOIN control_tunnel_nodes n ON n.id=a.edge_node_id AND n.endpoints_epoch=a.edge_process_epoch
 WHERE e.preview_lease_id=$2 AND e.account_id=$1 AND e.agent_machine_id=$3),
 EXISTS(SELECT 1 FROM preview_routes v WHERE v.endpoint AND v.preview_id=$2 AND v.account_id=$1 AND v.owner_machine_id=$3 AND v.owner_session_id=$4)`,
		accountID, previewID, ownerMachineID, ownerSessionID).Scan(&engine, &serving)
	if err != nil {
		return err
	}
	if !engine {
		return ErrNotServing
	}
	if !serving {
		return ErrNotServing
	}
	return nil
}
