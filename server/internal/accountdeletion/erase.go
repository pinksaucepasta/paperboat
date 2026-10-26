package accountdeletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// Outbox job kinds. The target is the WorkOS subject, the Stripe subscription, or the mail's fixed name.
const (
	jobWorkOSUser      = "workos_user"
	jobStripeCancel    = "stripe_subscription"
	jobConfirmation    = "confirmation_mail"
	confirmationMailID = "account-deleted"
)

// eraseSteps are the personal rows the deletion removes, revokes or stops. Each is scoped to the
// account in $1. Order matters: a row is removed after the rows that reference it.
var eraseSteps = []struct{ name, query string }{
	{"browser sessions", `UPDATE sessions SET revoked_at=coalesce(revoked_at,now()), workos_refresh_token_ciphertext=NULL, updated_at=now(), version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`},
	{"browser access sessions", `UPDATE browser_access_sessions SET revoked_at=coalesce(revoked_at,now()) WHERE (account_id=$1 OR owner_account_id=$1) AND revoked_at IS NULL`},
	{"CLI client sessions", `UPDATE cli_client_sessions SET state='revoked', revoked_at=coalesce(revoked_at,now()), revocation_reason='account_deleted' WHERE user_id=$1 AND state='active'`},
	{"CLI refresh tokens", `UPDATE cli_refresh_tokens SET state='revoked', revoked_at=coalesce(revoked_at,now()) WHERE state='active' AND cli_client_session_id IN (SELECT id FROM cli_client_sessions WHERE user_id=$1)`},
	{"CLI access tokens", `UPDATE cli_access_tokens SET revoked_at=coalesce(revoked_at,now()) WHERE revoked_at IS NULL AND cli_client_session_id IN (SELECT id FROM cli_client_sessions WHERE user_id=$1)`},
	{"API tokens", `UPDATE api_tokens SET revoked_at=now(), revoked_by='account_deleted' WHERE account_id=$1 AND revoked_at IS NULL`},
	{"device grants", `DELETE FROM device_grants WHERE user_id=$1`},
	{"managed SSH keys", `UPDATE managed_ssh_client_keys SET revoked_at=coalesce(revoked_at,now()) WHERE user_id=$1 AND revoked_at IS NULL`},
	{"browser terminal tickets", `DELETE FROM browser_terminal_tickets WHERE user_id=$1`},

	{"peer enrollment requests", `DELETE FROM peer_endpoint_enrollment_requests WHERE user_id=$1`},
	{"peer certificate operations", `DELETE FROM peer_endpoint_certificate_operations WHERE user_id=$1`},
	{"peer certificate revocations", `DELETE FROM peer_endpoint_certificate_revocations WHERE user_id=$1`},
	{"peer registration operations", `DELETE FROM peer_network_registration_operations WHERE user_id=$1`},
	// Identities reference their certificate (ON DELETE RESTRICT), so they go first.
	{"peer network identities", `DELETE FROM peer_network_identities WHERE user_id=$1`},
	{"peer endpoint certificates", `DELETE FROM peer_endpoint_certificates WHERE user_id=$1`},
	{"account E2EE keys", `DELETE FROM account_e2ee_keys WHERE user_id=$1`},
	{"account E2EE roots", `DELETE FROM account_e2ee_roots WHERE user_id=$1`},

	{"personal ENV scopes", `DELETE FROM environment_vault_scopes WHERE workspace_id='personal' AND owner_kind='personal' AND owner_id=$1`},
	{"personal ENV hosts", `DELETE FROM environment_vault_hosts WHERE account_id=$1`},
	{"personal ENV rotations", `DELETE FROM environment_vault_personal_rotation_scopes WHERE account_id=$1`},
	{"personal ENV rotations", `DELETE FROM environment_vault_personal_rotations WHERE account_id=$1`},
	{"personal ENV epochs", `DELETE FROM environment_vault_personal_epochs WHERE account_id=$1`},
	{"ENV operations", `DELETE FROM environment_vault_operations WHERE account_id=$1`},
	{"ENV account configuration", `DELETE FROM environment_vault_account_configurations WHERE account_id=$1`},
	{"personal password vaults", `DELETE FROM environment_password_vaults WHERE account_id=$1`},
	{"ENV key bindings", `DELETE FROM environment_key_bindings WHERE account_id=$1`},
	{"ENV key enrollments", `DELETE FROM environment_key_enrollment_requests WHERE account_id=$1`},
	{"ENV host bootstraps", `DELETE FROM environment_host_bootstraps WHERE account_id=$1`},
	{"ENV observations", `DELETE FROM environment_observations WHERE account_id=$1`},
	{"ENV scopes", `DELETE FROM environment_scopes WHERE account_id=$1`},
	{"ENV authority transitions", `DELETE FROM environment_authority_transitions WHERE account_id=$1`},
	{"ENV authority heads", `DELETE FROM environment_authority_heads WHERE account_id=$1`},
	{"ENV authority roots", `DELETE FROM environment_authority_roots WHERE account_id=$1`},
	{"ENV authorities", `DELETE FROM environment_authorities WHERE account_id=$1`},

	{"personal previews stopped", `UPDATE preview_leases SET allocation_state='released', edge_state='released', terminal_state='stopped', stopped_at=now() WHERE account_id=$1 AND owner_team_id IS NULL AND terminal_state='active'`},
	{"lazy activations", `DELETE FROM lazy_activations WHERE policy_id IN (SELECT id FROM lazy_access_policies WHERE account_id=$1)`},
	{"lazy access policies", `DELETE FROM lazy_access_policies WHERE account_id=$1`},
	{"lazy runtime owners", `DELETE FROM lazy_runtime_owners WHERE account_id=$1`},
	{"personal endpoints", `DELETE FROM endpoints WHERE account_id=$1 AND owner_team_id IS NULL`},

	{"personal tunnel routes", `UPDATE tunnel_routes SET desired_state='deleted', deleted_at=now(), updated_at=now(), generation=generation+1 WHERE desired_state<>'deleted' AND tunnel_id IN (SELECT id FROM tunnels WHERE account_id=$1 AND owner_team_id IS NULL)`},
	{"personal tunnel domains", `UPDATE tunnel_domains SET deleted_at=now(), updated_at=now(), generation=generation+1 WHERE account_id=$1 AND deleted_at IS NULL`},
	{"personal tunnels", `UPDATE tunnels SET desired_state='deleted', deleted_at=now(), updated_at=now(), generation=generation+1 WHERE account_id=$1 AND owner_team_id IS NULL AND deleted_at IS NULL`},
	{"tunnel logs", `DELETE FROM tunnel_log_entries WHERE account_id=$1`},

	{"config conflict resolutions", `DELETE FROM control_config_conflict_resolutions WHERE requested_by_user_id=$1`},
	{"team config default adoptions", `DELETE FROM team_config_default_adoptions WHERE account_id=$1`},
	{"personal config credentials", `DELETE FROM control_config_credentials WHERE machine_id IN (SELECT id FROM user_machines WHERE user_id=$1 AND owner_team_id IS NULL)`},
	{"personal config assignments", `DELETE FROM control_config_assignments WHERE machine_id IN (SELECT id FROM user_machines WHERE user_id=$1 AND owner_team_id IS NULL)`},
	{"config repositories", `DELETE FROM control_config_repositories WHERE owner_user_id=$1`},
	{"config environments", `UPDATE control_environments SET owner_user_id=NULL, desired_state='revoked', revoked_at=coalesce(revoked_at,now()), updated_at=now() WHERE owner_user_id=$1`},
	{"config compare admissions", `DELETE FROM config_compare_admissions WHERE account_id=$1`},

	{"team inbox receipts", `DELETE FROM team_inbox_receipt_outbox WHERE recipient_account=$1`},
	{"team inbox requests", `DELETE FROM team_inbox_requests WHERE sender_account=$1 OR recipient_account=$1`},
	{"team inbox policies", `DELETE FROM team_inbox_policies WHERE account_id=$1`},
	{"team inbox sender policies", `DELETE FROM team_inbox_sender_policies WHERE sender_account=$1 OR recipient_account=$1`},

	{"GitHub native links", `DELETE FROM github_native_links WHERE user_id=$1`},
	{"GitHub repository provisioning", `DELETE FROM github_repo_provisioning_attempts WHERE user_id=$1`},
	{"GitHub OAuth tokens", `DELETE FROM github_oauth_tokens WHERE user_id=$1`},

	{"billing portal operations", `DELETE FROM billing_portal_operations WHERE user_id=$1`},
	{"billing checkout reservations", `DELETE FROM billing_checkout_reservations WHERE user_id=$1`},

	{"favorites", `DELETE FROM user_favorites WHERE user_id=$1`},
	{"transfer destination defaults", `DELETE FROM user_transfer_destination_defaults WHERE user_id=$1`},
	{"browser viewer policies", `DELETE FROM browser_viewer_policies WHERE owner_account_id=$1`},
	{"identity links", `DELETE FROM user_identities WHERE user_id=$1`},
}

// eraseTx performs the whole deletion. It returns OwnedTeamsError before changing anything when the
// account owns a team with other members; any other error rolls the whole deletion back.
func (s *Service) eraseTx(ctx context.Context, tx *db.Tx, userID string) error {
	// Team mutations take the team lock before the vault lock; keep that order.
	if err := teams.Lock(ctx, tx); err != nil {
		return fmt.Errorf("account deletion: lock teams: %w", err)
	}
	if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
		return fmt.Errorf("account deletion: lock vaults: %w", err)
	}
	subject, email, err := lockActiveUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	owned, err := ownedTeamsTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	var blocking []TeamBlock
	var deletedTeams []string
	for _, t := range owned {
		if t.Members > 1 {
			blocking = append(blocking, t)
		} else {
			deletedTeams = append(deletedTeams, t.Slug)
		}
	}
	if len(blocking) > 0 {
		return &OwnedTeamsError{Teams: blocking}
	}
	if err := s.leaveTeamsTx(ctx, tx, userID, deletedTeams); err != nil {
		return err
	}
	if err := s.eraseMachinesTx(ctx, tx, userID); err != nil {
		return err
	}
	for _, step := range eraseSteps {
		if _, err := tx.Exec(ctx, step.query, userID); err != nil {
			return fmt.Errorf("account deletion: remove %s: %w", step.name, err)
		}
	}
	if err := anonymizeAuditTx(ctx, tx, userID); err != nil {
		return err
	}
	if err := queueProviderJobsTx(ctx, tx, userID, subject, email, deletedTeams); err != nil {
		return err
	}
	// The row stays: billing and audit reference it. It holds no name, email or sign-in identity.
	if _, err := tx.Exec(ctx, `UPDATE users SET status='deleted', role='user', primary_email='deleted:'||id, canonical_email=NULL,
		display_name='', email_verified=false, workos_subject='deleted:'||id, updated_at=now(), version=version+1 WHERE id=$1`, userID); err != nil {
		return fmt.Errorf("account deletion: mark user deleted: %w", err)
	}
	return nil
}

func lockActiveUserTx(ctx context.Context, tx *db.Tx, userID string) (subject, email string, err error) {
	var status string
	err = tx.QueryRow(ctx, `SELECT workos_subject, primary_email, status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&subject, &email, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrAccountNotActive
	}
	if err != nil {
		return "", "", fmt.Errorf("account deletion: lock account: %w", err)
	}
	if status != "active" {
		return "", "", ErrAccountNotActive
	}
	return subject, email, nil
}

// ownedTeamsTx is ownedTeams inside the deletion transaction.
func ownedTeamsTx(ctx context.Context, tx *db.Tx, userID string) ([]TeamBlock, error) {
	rows, err := tx.Query(ctx, `SELECT team_id, coalesce(display_name,'') FROM teams WHERE owner_account=$1 AND deleted_at IS NULL ORDER BY team_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("account deletion: list owned teams: %w", err)
	}
	var out []TeamBlock
	for rows.Next() {
		var t TeamBlock
		if err := rows.Scan(&t.Slug, &t.Name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("account deletion: read owned team: %w", err)
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account deletion: list owned teams: %w", err)
	}
	for i := range out {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1 AND active`, out[i].Slug).Scan(&out[i].Members); err != nil {
			return nil, fmt.Errorf("account deletion: count team members: %w", err)
		}
		if out[i].Name == "" {
			out[i].Name = out[i].Slug
		}
	}
	return out, nil
}

// leaveTeamsTx deletes the single-member teams the account owns and removes the account from every
// other team it is in, with the same revocations the team actions use.
func (s *Service) leaveTeamsTx(ctx context.Context, tx *db.Tx, userID string, deletedTeams []string) error {
	for _, team := range deletedTeams {
		if err := teams.DeleteTeamTx(ctx, tx, team); err != nil {
			return fmt.Errorf("account deletion: delete team: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT m.team_id FROM team_members m JOIN teams t ON t.team_id=m.team_id
		WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL AND t.owner_account<>$1 ORDER BY m.team_id`, userID)
	if err != nil {
		return fmt.Errorf("account deletion: list team memberships: %w", err)
	}
	var memberships []string
	for rows.Next() {
		var team string
		if err := rows.Scan(&team); err != nil {
			rows.Close()
			return fmt.Errorf("account deletion: read team membership: %w", err)
		}
		memberships = append(memberships, team)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account deletion: list team memberships: %w", err)
	}
	for _, team := range memberships {
		if err := teams.RemoveMemberTx(ctx, tx, team, userID); err != nil {
			return fmt.Errorf("account deletion: leave team: %w", err)
		}
	}
	return nil
}

// eraseMachinesTx deletes every personal machine through the same revocation as the delete action.
func (s *Service) eraseMachinesTx(ctx context.Context, tx *db.Tx, userID string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM user_machines WHERE user_id=$1 AND owner_team_id IS NULL AND deleted_at IS NULL ORDER BY id`, userID)
	if err != nil {
		return fmt.Errorf("account deletion: list machines: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("account deletion: read machine: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account deletion: list machines: %w", err)
	}
	for _, id := range ids {
		machine, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: id, UserID: userID})
		if err != nil {
			return fmt.Errorf("account deletion: lock machine: %w", err)
		}
		if err := s.machines.DeleteMachineTx(ctx, tx, userID, machine); err != nil {
			return fmt.Errorf("account deletion: delete machine: %w", err)
		}
	}
	return nil
}

// anonymizeAuditTx clears the actor of every audit event the account performed. The events stay,
// shown as a deleted user; the append-only trigger allows only this actor change.
func anonymizeAuditTx(ctx context.Context, tx *db.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('paperboat.audit_anonymize', 'on', true)`); err != nil {
		return fmt.Errorf("account deletion: enable audit anonymization: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_events SET actor_type='deleted_user', actor_user_id=NULL, actor_email=NULL, actor_id=NULL,
		account_id=CASE WHEN account_id=$1 THEN NULL ELSE account_id END WHERE actor_user_id=$1`, userID); err != nil {
		return fmt.Errorf("account deletion: anonymize audit events: %w", err)
	}
	return nil
}

// queueProviderJobsTx records the provider work that runs after commit: the WorkOS user, every
// subscription still active on the account or on a team it deleted, and the confirmation mail.
func queueProviderJobsTx(ctx context.Context, tx *db.Tx, userID, subject, email string, deletedTeams []string) error {
	if subject != "" {
		if err := queueJobTx(ctx, tx, userID, jobWorkOSUser, subject, ""); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT provider_subscription_id FROM billing_workspaces
		WHERE (user_id=$1 OR team_id=ANY($2::text[])) AND provider_subscription_id IS NOT NULL AND state NOT IN ('canceled','incomplete_expired')
		ORDER BY provider_subscription_id`, userID, deletedTeams)
	if err != nil {
		return fmt.Errorf("account deletion: list subscriptions: %w", err)
	}
	var subscriptions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("account deletion: read subscription: %w", err)
		}
		subscriptions = append(subscriptions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account deletion: list subscriptions: %w", err)
	}
	for _, id := range subscriptions {
		if err := queueJobTx(ctx, tx, userID, jobStripeCancel, id, ""); err != nil {
			return err
		}
	}
	return queueJobTx(ctx, tx, userID, jobConfirmation, confirmationMailID, email)
}

func queueJobTx(ctx context.Context, tx *db.Tx, userID, kind, target, recipient string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account_deletion_jobs (id,user_id,kind,target,recipient) VALUES ($1,$2,$3,$4,$5)`,
		uuid.NewString(), userID, kind, target, recipient); err != nil {
		return fmt.Errorf("account deletion: queue %s: %w", kind, err)
	}
	return nil
}
