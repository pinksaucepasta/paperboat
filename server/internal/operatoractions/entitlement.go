package operatoractions

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

func isEntitlementKind(kind string) bool {
	return kind == "entitlement_grant" || kind == "entitlement_revoke"
}

// entitlementAction gives a workspace extra bandwidth or an override of one plan limit, or takes a grant back. The target is
// the workspace id (personal:<account> or team:<team>). What the grant changes is enforced by planlimits and billing, which
// read the grants table; this only writes it, inside the receipt's transaction, so the audit event and the grant commit together.
func entitlementAction(ctx context.Context, tx *db.Tx, actor, kind, target, reason string, in Input, out *Result) error {
	if kind == "entitlement_revoke" {
		g, err := planlimits.RevokeTx(ctx, tx, actor, target, in.GrantID)
		if err != nil {
			return mapEntitlementError(err)
		}
		*out = Result{ResourceID: target, State: "revoked", GrantID: g.ID}
		return nil
	}
	var endsAt *time.Time
	if in.EndsAt != "" {
		t, err := time.Parse(time.RFC3339, in.EndsAt)
		if err != nil {
			return ErrInvalid
		}
		t = t.UTC()
		endsAt = &t
	}
	if planlimits.Kind(in.Entitlement) == planlimits.Seats && in.Amount > teams.MaximumMembers {
		return ErrInvalid // no team can hold more members than the platform maximum, so a larger grant would never take effect
	}
	g, err := planlimits.GrantTx(ctx, tx, actor, target, planlimits.Kind(in.Entitlement), in.Amount, endsAt, reason)
	if err != nil {
		return mapEntitlementError(err)
	}
	*out = Result{ResourceID: target, State: "granted", GrantID: g.ID, ExpiresAt: endsAt}
	return nil
}

func mapEntitlementError(err error) error {
	switch {
	case errors.Is(err, planlimits.ErrGrant):
		return ErrInvalid
	case errors.Is(err, planlimits.ErrGrantNotFound):
		return ErrNotFound
	}
	return err
}
