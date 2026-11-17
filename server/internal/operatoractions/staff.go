package operatoractions

import (
	"context"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

// Operator management kinds act on operator accounts, not customer resources: they take no revision (operators
// have none) and only an admin may run them.
func isStaffKind(kind string) bool { return strings.HasPrefix(kind, "operator_") }

// auditResource is what the audit event is about: for an invitation the new operator, otherwise the target.
func auditResource(kind, target string, result Result) string {
	if kind == "operator_create" {
		return result.ResourceID
	}
	return target
}

// inputFits refuses what a kind does not take: an operator action has no revision, no legal flag, and only the
// fields of its own kind; every other kind needs a revision and none of the operator fields.
func inputFits(kind string, in Input) bool {
	if !isStaffKind(kind) {
		return in.Revision >= 1 && in.Email == "" && in.Role == "" && in.SessionID == ""
	}
	create, revoke := kind == "operator_create", kind == "operator_sessions_revoke"
	return in.Revision == 0 && !in.Legal && (create || (in.Email == "" && in.Role == "")) && (revoke || in.SessionID == "") && (!revoke || in.SessionID != "") && (!create || (in.Email != "" && in.Role != ""))
}

func staffAction(ctx context.Context, tx *db.Tx, actor, kind, target string, in Input, out *Result) error {
	var err error
	switch kind {
	case "operator_create":
		var invitation operatorauth.Invitation
		if invitation, err = operatorauth.InviteTx(ctx, tx, in.Email, in.Role); err == nil {
			expires := invitation.ExpiresAt
			*out = Result{ResourceID: invitation.OperatorID, State: "invited", SetupToken: invitation.Token, ExpiresAt: &expires}
		}
	case "operator_disable", "operator_enable":
		var state string
		if state, err = operatorauth.SetDisabledTx(ctx, tx, actor, target, kind == "operator_disable"); err == nil {
			*out = Result{ResourceID: target, State: state}
		}
	case "operator_mfa_reset":
		if err = operatorauth.ResetMFATx(ctx, tx, actor, target); err == nil {
			*out = Result{ResourceID: target, State: "mfa_reset"}
		}
	case "operator_sessions_revoke":
		var revoked int64
		if revoked, err = operatorauth.RevokeSessionsTx(ctx, tx, actor, target, in.SessionID); err == nil {
			*out = Result{ResourceID: target, State: "sessions_revoked", Revoked: revoked}
		}
	}
	return mapStaffError(err)
}

func mapStaffError(err error) error {
	switch {
	case err == nil:
		return nil
	case operatorExpectedErrorOnly(err, operatorauth.ErrForbidden):
		return ErrForbidden
	case operatorExpectedErrorOnly(err, operatorauth.ErrNotFound):
		return ErrNotFound
	case operatorExpectedErrorOnly(err, operatorauth.ErrConflict):
		return ErrConflict
	case operatorExpectedErrorOnly(err, operatorauth.ErrInvalid):
		return ErrInvalid
	}
	return err
}
