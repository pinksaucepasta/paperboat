package peersessions

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// RelayDeniedQuotaExhausted is the typed reason a network refresh carries when hosted relay
// grants were withheld because the account has no bandwidth allowance left anywhere.
const RelayDeniedQuotaExhausted = "bandwidth_quota_exhausted"

// ConfigureQuota makes relay grants admission-checked: a hosted relay grant is only signed
// while at least one workspace the account can use (its personal workspace or a team it
// belongs to) still has allowance. billingURL is the console's Billing page, published to the
// client so it can tell the user where to add bandwidth; it may be empty.
func (s *NetworkService) ConfigureQuota(quota accountingauthority.QuotaChecker, billingURL string) {
	s.quota, s.billingURL = quota, billingURL
}

// relayQuotaGateEnabled switches the per-refresh allowance check. Admission and debit still
// enforce the cap; only the early withholding of relay grants is off.
const relayQuotaGateEnabled = false

// relayQuotaWithheld is the network-configuration gate. Peer networking must never depend on
// billing availability: when the allowance cannot be read, relay grants stay available (flows
// are still metered and charged when they are admitted) and the failure is logged.
func (s *NetworkService) relayQuotaWithheld(ctx context.Context, account string, at time.Time) bool {
	// Disabled until the check is lock-free and cached: running it in the refresh path wrote
	// and locked billing rows on every refresh and took peer networking down under load.
	if !relayQuotaGateEnabled {
		return false
	}
	denied, err := s.relayQuotaDenied(ctx, account, at)
	if err != nil {
		slog.WarnContext(ctx, "relay quota check unavailable; relay grants stay available", "error_type", fmt.Sprintf("%T", err))
		return false
	}
	return denied
}

// relayQuotaDenied reports whether every workspace available to the account is exhausted.
// A grant names no workspace (relays carry opaque packets), so the check is per account: one
// workspace with allowance left keeps relays available, and the traffic is then charged to
// the workspace that admitted each flow. Established flows end when their short grant expires.
func (s *NetworkService) relayQuotaDenied(ctx context.Context, account string, at time.Time) (bool, error) {
	if s.quota == nil || account == "" {
		return false, nil
	}
	denied := true
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		workspaces := []string{"personal:" + account}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, workspaces[0], account); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT t.team_id FROM teams t JOIN team_members m ON m.team_id=t.team_id AND m.active
WHERE m.account_id=$1 AND t.deleted_at IS NULL ORDER BY t.team_id LIMIT 64`, account)
		if err != nil {
			return err
		}
		for rows.Next() {
			var team string
			if err := rows.Scan(&team); err != nil {
				rows.Close()
				return err
			}
			workspaces = append(workspaces, "team:"+team)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, workspace := range workspaces {
			if workspace != "personal:"+account {
				if _, err := tx.Exec(ctx, `INSERT INTO billing_workspaces(id,team_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, workspace, workspace[len("team:"):]); err != nil {
					return err
				}
			}
			exhausted, err := s.quota.CheckWorkspaceBandwidthTx(ctx, tx, workspace, at)
			if err != nil {
				return err
			}
			if !exhausted {
				denied = false
				return nil
			}
		}
		return nil
	})
	return denied, err
}
