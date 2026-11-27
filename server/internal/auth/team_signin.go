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
	// name, which is the account's own exact address (F13-07, first sign-up wins).
	if profile.EmailVerified {
		if err := teams.AcceptDirectoryInvitationsTx(ctx, tx, account, strings.ToLower(strings.TrimSpace(profile.Email))); err != nil {
			return err
		}
	}
	if profile.AuthenticationMethod != "SSO" || profile.OrganizationID == "" {
		return nil
	}
	return teams.RecordSSOSignInTx(ctx, tx, profile.OrganizationID, account, email)
}
