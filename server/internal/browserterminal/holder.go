package browserterminal

import (
	"context"
	"errors"
	"net/url"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// CarrierHolder answers Gateway.HoldsCarrier from the runtime carrier attachments. The route's validity (machine,
// node and epoch still current, node ready and not past its drain deadline, carrier observed within the freshness
// window) is paperboat.runtime_carrier_valid's: it is called, never copied, so it stays one definition.
type CarrierHolder struct{ DB *db.DB }

// Holds reports whether the edge process holds a ready, valid carrier for a runtime route of the decision's
// environment whose managed hostname is the one in the decision's browser terminal URL.
func (h CarrierHolder) Holds(ctx context.Context, d Decision, nodeID, processEpoch string) (bool, error) {
	if h.DB == nil || d.EnvironmentID == "" || nodeID == "" || processEpoch == "" {
		return false, errors.New("browser terminal carrier holder check is not configured")
	}
	u, err := url.Parse(d.HostWSSURL)
	if err != nil || u.Hostname() == "" {
		return false, nil
	}
	var holds bool
	err = h.DB.Pool().QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM control_routes r
WHERE r.environment_id=$1 AND r.kind='runtime_https_wss' AND r.managed_public_host=$2
 AND paperboat.runtime_carrier_valid(r.id,$3,$4,true))`, d.EnvironmentID, u.Hostname(), nodeID, processEpoch).Scan(&holds)
	return holds, err
}
