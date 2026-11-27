package auth

import (
	"context"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// recordTeamSignInTx applies a sign-in to teams (PLAN F19-03/05): held directory invitations for
// the verified email become membership, and an SSO sign-in through a team's WorkOS organization
// satisfies that team's Require SSO (and queues JIT provisioning where the team allows it).
func recordTeamSignInTx(ctx context.Context, tx *db.Tx, account, email string, profile WorkOSProfile) error {
	// Directory invitations are auto-accepted only for the exact address they
	// name: a +tag can be a different mailbox from x (F13-07, L36).
	if profile.EmailVerified && strings.EqualFold(strings.TrimSpace(profile.Email), email) {
		if err := teams.AcceptDirectoryInvitationsTx(ctx, tx, account, email); err != nil {
			return err
		}
	}
	if profile.AuthenticationMethod != "SSO" || profile.OrganizationID == "" {
		return nil
	}
	return teams.RecordSSOSignInTx(ctx, tx, profile.OrganizationID, account, email)
}
