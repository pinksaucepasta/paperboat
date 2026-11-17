package operatorauth

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Operator management. The statements below run inside the caller's transaction (operatoractions.Execute, which
// owns the receipt, the idempotency and the audit event), so each action and its receipt commit together.
// Prior art: netbird management/server/user.go (an admin cannot block themselves; blocking ends the user's
// sessions at once) and teleport lib/auth/accountrecovery.go (single-use token starts the account flow).

var (
	ErrNotFound  = errors.New("operator not found")
	ErrConflict  = errors.New("operator state conflict")
	ErrForbidden = errors.New("operator action forbidden")
)

const (
	inviteLifetime = 72 * time.Hour
	// unsetPassword is stored until the invited operator chooses one; verifyPassword rejects it for every input.
	unsetPassword = "!"
	// adminsLock serialises everything that can remove an active admin, so the last one cannot be removed twice.
	adminsLock = `SELECT pg_advisory_xact_lock(hashtextextended('operator_admins', 714215))`
)

// Invitation is the one-time setup token an admin hands to a new operator out of band. Only its hash is stored.
type Invitation struct {
	OperatorID string
	Token      string
	ExpiresAt  time.Time
}

func validRole(role string) bool {
	return role == "admin" || role == "operations" || role == "billing" || role == "support"
}

// InviteTx creates the operator (no usable password) and a fresh setup token. For an operator who was invited and
// has not finished setup it issues a new token instead, so a lost token is recovered by inviting again.
func InviteTx(ctx context.Context, tx *db.Tx, email, role string) (Invitation, error) {
	email = canonicalEmail(email)
	if len(email) > 254 || strings.ContainsAny(email, " \t\r\n") || strings.Count(email, "@") != 1 || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") || !validRole(role) {
		return Invitation{}, ErrInvalid
	}
	token, err := randomToken()
	if err != nil {
		return Invitation{}, err
	}
	invitation := Invitation{Token: token, ExpiresAt: time.Now().UTC().Add(inviteLifetime)}
	var existingHash, status string
	err = tx.QueryRow(ctx, `SELECT id,password_hash,status FROM paperboat.operator_accounts WHERE email=$1 FOR UPDATE`, email).Scan(&invitation.OperatorID, &existingHash, &status)
	switch {
	case err == nil:
		if existingHash != unsetPassword || status != "active" {
			return Invitation{}, ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET role=$2 WHERE id=$1`, invitation.OperatorID, role); err != nil {
			return Invitation{}, err
		}
	case operatorAuthNoRowsOnly(err):
		id, idErr := uuid.NewRandom()
		if idErr != nil {
			return Invitation{}, idErr
		}
		invitation.OperatorID = "operator_" + id.String()
		tag, insertErr := tx.Exec(ctx, `INSERT INTO paperboat.operator_accounts(id,email,password_hash,role) VALUES($1,$2,$3,$4) ON CONFLICT (email) DO NOTHING`, invitation.OperatorID, email, unsetPassword, role)
		if insertErr != nil {
			return Invitation{}, insertErr
		}
		if tag.RowsAffected() == 0 {
			return Invitation{}, ErrConflict // another invitation for this email committed first; invite again to replace its token
		}
	default:
		return Invitation{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.operator_invitations(operator_id,token_hash,expires_at) VALUES($1,$2,$3)
 ON CONFLICT (operator_id) DO UPDATE SET token_hash=EXCLUDED.token_hash,expires_at=EXCLUDED.expires_at,created_at=now()`, invitation.OperatorID, hash(token), invitation.ExpiresAt); err != nil {
		return Invitation{}, err
	}
	return invitation, nil
}

// lockOperator returns the target's role and status with its row locked.
func lockOperator(ctx context.Context, tx *db.Tx, id string) (role, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT role,status FROM paperboat.operator_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&role, &status)
	if operatorAuthNoRowsOnly(err) {
		return "", "", ErrNotFound
	}
	return role, status, err
}

func revokeAll(ctx context.Context, tx *db.Tx, id string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE paperboat.operator_sessions SET revoked_at=now() WHERE operator_id=$1 AND revoked_at IS NULL`, id)
	return tag.RowsAffected(), err
}

// SetDisabledTx disables or re-enables an operator. Every sign-in and request path already requires
// status='active', so a disabled operator fails closed; their live sessions are ended here in the same transaction.
// Enabling does not bring sessions back. Self-disable and removing the last active admin are refused.
func SetDisabledTx(ctx context.Context, tx *db.Tx, actor, target string, disable bool) (state string, err error) {
	if disable && actor == target {
		return "", ErrForbidden
	}
	if _, err = tx.Exec(ctx, adminsLock); err != nil {
		return "", err
	}
	role, status, err := lockOperator(ctx, tx, target)
	if err != nil {
		return "", err
	}
	if !disable {
		if status != "active" {
			_, err = tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET status='active',failed_attempts=0,locked_until=NULL WHERE id=$1`, target)
		}
		return "active", err
	}
	if status == "active" && role == "admin" {
		var others int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM paperboat.operator_accounts WHERE role='admin' AND status='active' AND id<>$1`, target).Scan(&others); err != nil {
			return "", err
		}
		if others == 0 {
			return "", ErrConflict
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET status='disabled' WHERE id=$1`, target); err != nil {
		return "", err
	}
	_, err = revokeAll(ctx, tx, target)
	return "disabled", err
}

// ResetMFATx deletes the operator's authenticator and recovery codes and ends all their sessions (a signed-in
// session must not outlive its factor). The next sign-in enrols a new authenticator. Resetting your own is refused.
func ResetMFATx(ctx context.Context, tx *db.Tx, actor, target string) error {
	if actor == target {
		return ErrForbidden
	}
	if _, _, err := lockOperator(ctx, tx, target); err != nil {
		return err
	}
	// A lockout from failed codes ends with the factor it guarded, so the operator can enrol at their next sign-in.
	if _, err := tx.Exec(ctx, `UPDATE paperboat.operator_accounts SET failed_attempts=0,locked_until=NULL WHERE id=$1`, target); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.operator_totp WHERE operator_id=$1`, target); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.operator_recovery_codes WHERE operator_id=$1`, target); err != nil {
		return err
	}
	_, err := revokeAll(ctx, tx, target)
	return err
}

// RevokeSessionsTx ends one session (its id from the sessions list) or, for "all", every live session of the
// operator. Ending your own is refused (that is sign out), so an admin cannot lose the page they are acting from.
func RevokeSessionsTx(ctx context.Context, tx *db.Tx, actor, target, sessionID string) (int64, error) {
	if actor == target {
		return 0, ErrForbidden
	}
	if _, _, err := lockOperator(ctx, tx, target); err != nil {
		return 0, err
	}
	if sessionID == "all" {
		return revokeAll(ctx, tx, target)
	}
	id, err := hex.DecodeString(sessionID)
	if err != nil || len(id) != 32 {
		return 0, ErrInvalid
	}
	tag, err := tx.Exec(ctx, `UPDATE paperboat.operator_sessions SET revoked_at=now() WHERE operator_id=$1 AND token_hash=$2 AND revoked_at IS NULL AND expires_at>now()`, target, id)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	return 1, nil
}

// SetupInvited finishes an invitation: the operator proves the token, chooses a password and gets a pending
// session that must enrol an authenticator. The token is consumed in the same transaction (single use), and every
// failure is the same ErrInvalid.
func (s *Service) SetupInvited(ctx context.Context, email, token, password string) (session Session, resultErr error) {
	email = canonicalEmail(email)
	// A wrong or expired token is refused before the password hash is computed, so garbage requests stay cheap.
	var valid bool
	if err := s.Store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.operator_accounts a JOIN paperboat.operator_invitations i ON i.operator_id=a.id
 WHERE a.email=$1 AND a.status='active' AND a.password_hash=$2 AND i.token_hash=$3 AND i.expires_at>now())`, email, unsetPassword, hash(token)).Scan(&valid); err != nil {
		return Session{}, err
	}
	if !valid {
		return Session{}, ErrInvalid
	}
	encoded, err := passwordHash(password)
	if err != nil {
		return Session{}, err
	}
	sessionToken, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	tx, err := s.Store.SQL().BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer captureOperatorAuthRollback(tx, &resultErr)
	account := Account{Email: email}
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.role FROM paperboat.operator_accounts a JOIN paperboat.operator_invitations i ON i.operator_id=a.id
 WHERE a.email=$1 AND a.status='active' AND a.password_hash=$2 AND i.token_hash=$3 AND i.expires_at>now() FOR UPDATE OF a,i`, email, unsetPassword, hash(token)).Scan(&account.ID, &account.Role)
	if operatorAuthNoRowsOnly(err) {
		return Session{}, ErrInvalid
	}
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(pendingLifetime)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE paperboat.operator_accounts SET password_hash=$2,failed_attempts=0,locked_until=NULL WHERE id=$1`, []any{account.ID, encoded}},
		{`DELETE FROM paperboat.operator_invitations WHERE operator_id=$1`, []any{account.ID}},
		{`INSERT INTO paperboat.operator_sessions(token_hash,operator_id,csrf_hash,expires_at,mfa_pending) VALUES($1,$2,$3,$4,true)`, []any{hash(sessionToken), account.ID, hash(csrf), expires}},
	} {
		if _, err = tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return Session{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	account.AuthenticatedAt = now
	return Session{Token: sessionToken, CSRF: csrf, ExpiresAt: expires, Account: account, MFA: MFAEnroll}, nil
}
