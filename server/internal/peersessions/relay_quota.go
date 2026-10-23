package peersessions

import (
	"context"
	"database/sql"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
)

// RelayDeniedQuotaExhausted is the typed reason a network refresh carries when hosted relay
// grants were withheld because the account has no bandwidth allowance left anywhere.
const RelayDeniedQuotaExhausted = "bandwidth_quota_exhausted"

// ConfigureQuota makes hosted relay grants admission-checked against the account's remaining
// bandwidth (internal/bandwidthgate: read-only, cached, fail-open). billingURL is the console's
// Billing page, published to the client; it may be empty.
func (s *NetworkService) ConfigureQuota(billingURL string) {
	s.quotaGate, s.billingURL = bandwidthgate.New(s.store), billingURL
}

// billableRelayPeers keeps the peers whose connection may use Paperboat's hosted relays: the
// workspace that owns the connection (a team when either machine is a team's, otherwise the
// account's personal workspace) still has relay and edge allowance. The decision is per
// workspace, so a member of a team with allowance left gets no free relay for a capped personal
// workspace, and the reverse. The gate is read-only, cached and never blocks (bandwidthgate).
func (s *NetworkService) billableRelayPeers(ctx context.Context, tx *sql.Tx, self networkBinding, peers []networkPeer, at time.Time) ([]networkPeer, error) {
	if s.quotaGate == nil || len(peers) == 0 {
		return peers, nil
	}
	ids := make([]string, 0, len(peers)+1)
	ids = append(ids, self.MachineID)
	for _, peer := range peers {
		ids = append(ids, peer.Identity.MachineID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,coalesce(owner_team_id,'') FROM paperboat.user_machines WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	teamOf := make(map[string]string, len(ids))
	for rows.Next() {
		var id, team string
		if err := rows.Scan(&id, &team); err != nil {
			rows.Close()
			return nil, err
		}
		teamOf[id] = team
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	billable := make([]networkPeer, 0, len(peers))
	for _, peer := range peers {
		workspace := "personal:" + self.AccountID
		if team := teamOf[peer.Identity.MachineID]; team != "" {
			workspace = "team:" + team
		} else if team := teamOf[self.MachineID]; team != "" {
			workspace = "team:" + team
		}
		if !s.quotaGate.Denied(ctx, workspace, at) {
			billable = append(billable, peer)
		}
	}
	return billable, nil
}
