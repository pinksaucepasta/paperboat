package browserterminal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var ErrTicketInvalid = errors.New("browser terminal ticket is invalid or expired")

const TicketLifetime = 30 * time.Second

type Ticket struct {
	Value                  string
	GuestSessionID         string
	UserID                 string
	BrowserSessionID       string
	TerminalSessionID      string
	InspectorCredentialID  string
	Origin                 string
	Role                   string
	PolicyGeneration       int64
	TeamGeneration         int64
	MembershipGeneration   int64
	BindingGeneration      int64
	BrowserPublicKeySHA256 string
	ExpiresAt              time.Time
}

type TicketStore struct{ DB *db.DB }

// Issue creates a short-lived, single-use admission for an already authorized
// browser account. The browser's Paperboat session is checked again at redemption.
func (s TicketStore) Issue(ctx context.Context, in Ticket) (result Ticket, resultErr error) {
	if s.DB == nil || !in.validPrincipal() || !in.validResource() || in.Origin == "" || in.PolicyGeneration < 1 || in.TeamGeneration < 0 || in.MembershipGeneration < 0 || in.BindingGeneration < 0 || !validRole(in.Role) || !validPublicKeySHA256(in.BrowserPublicKeySHA256) {
		return Ticket{}, ErrTicketInvalid
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Ticket{}, err
	}
	in.Value = base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256(secret)
	in.ExpiresAt = time.Now().UTC().Add(TicketLifetime)
	_, err := s.DB.SQL().ExecContext(ctx, `DELETE FROM browser_terminal_tickets WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return Ticket{}, err
	}
	tx, err := s.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr == nil || rollbackErr == sql.ErrTxDone {
			return
		}
		result = Ticket{}
		resultErr = ticketTransactionCleanupError{operation: resultErr, cleanup: rollbackErr}
	}()
	if in.GuestSessionID != "" {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT c.enabled AND c.expires_at>now() AND g.expires_at>now() AND c.generation=g.code_generation
   AND c.generation=$2 AND c.role=$3 FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
   WHERE g.id=$1 AND g.terminal_session_id=$4 FOR UPDATE OF g`, in.GuestSessionID, in.PolicyGeneration, in.Role, in.TerminalSessionID).Scan(&active)
		if err == sql.ErrNoRows {
			return Ticket{}, ErrTicketInvalid
		}
		if err != nil {
			return Ticket{}, err
		}
		if !active {
			return Ticket{}, ErrTicketInvalid
		}
	} else {
		// Serialize issue requests on the browser session so the pending limit is strict.
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT account.status='active' AND auth_session.revoked_at IS NULL AND auth_session.expires_at>now()
		FROM sessions auth_session JOIN users account ON account.id=auth_session.user_id
		WHERE auth_session.id=$1 AND auth_session.user_id=$2 FOR UPDATE OF auth_session`, in.BrowserSessionID, in.UserID).Scan(&active)
		if err == sql.ErrNoRows {
			return Ticket{}, ErrTicketInvalid
		}
		if err != nil {
			return Ticket{}, err
		}
		if !active {
			return Ticket{}, ErrTicketInvalid
		}
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM browser_terminal_tickets WHERE (browser_session_id=$1 OR guest_session_id=$1) AND consumed_at IS NULL AND expires_at>now()`, in.principalSessionID()).Scan(&pending); err != nil {
		return Ticket{}, err
	}
	if pending >= 8 {
		return Ticket{}, ErrTicketInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO browser_terminal_tickets (token_hash,user_id,browser_session_id,terminal_session_id,origin,role,policy_generation,team_generation,membership_generation,binding_generation,browser_public_key_sha256,expires_at,guest_session_id,inspector_credential_id)
		VALUES ($1,nullif($2,''),nullif($3,''),nullif($4,''),$5,$6,$7,$8,$9,$10,$11,$12,nullif($13,''),nullif($14,''))`, hash[:], in.UserID, in.BrowserSessionID, in.TerminalSessionID, in.Origin, in.Role, in.PolicyGeneration, in.TeamGeneration, in.MembershipGeneration, in.BindingGeneration, in.BrowserPublicKeySHA256, in.ExpiresAt, in.GuestSessionID, in.InspectorCredentialID); err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return in, nil
}

// Consume checks the exact browser Origin and atomically marks the ticket used.
// The caller must recheck the current resource policy and terminal state before
// connecting to a host; a ticket is not a durable grant.
func (s TicketStore) Consume(ctx context.Context, value, origin, terminalSessionID string) (Ticket, error) {
	if terminalSessionID == "" {
		return Ticket{}, ErrTicketInvalid
	}
	return s.consume(ctx, value, origin, terminalSessionID)
}

// ConsumeEdge resolves the terminal from the one-use secret. The edge need not
// learn or supply a resource identifier before the control plane checks it.
func (s TicketStore) ConsumeEdge(ctx context.Context, value, origin string) (Ticket, error) {
	return s.consume(ctx, value, origin, "")
}

func (s TicketStore) consume(ctx context.Context, value, origin, terminalSessionID string) (Ticket, error) {
	if s.DB == nil || origin == "" {
		return Ticket{}, ErrTicketInvalid
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != value {
		return Ticket{}, ErrTicketInvalid
	}
	hash := sha256.Sum256(secret)
	var out Ticket
	err = s.DB.SQL().QueryRowContext(ctx, `UPDATE browser_terminal_tickets ticket SET consumed_at=now()
		WHERE ticket.token_hash=$1 AND ticket.origin=$2 AND ($3='' OR ticket.terminal_session_id=$3)
  AND ticket.consumed_at IS NULL AND ticket.expires_at>now()
  AND ((ticket.guest_session_id IS NULL AND EXISTS(
   SELECT 1 FROM sessions auth_session JOIN users account ON account.id=auth_session.user_id
   WHERE auth_session.id=ticket.browser_session_id AND auth_session.user_id=ticket.user_id
   AND auth_session.revoked_at IS NULL AND auth_session.expires_at>now() AND account.status='active'))
   OR (ticket.guest_session_id IS NOT NULL AND EXISTS(
    SELECT 1 FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
    WHERE g.id=ticket.guest_session_id AND g.terminal_session_id=ticket.terminal_session_id AND g.expires_at>now()
    AND c.enabled AND c.expires_at>now() AND c.generation=g.code_generation AND c.generation=ticket.policy_generation AND c.role=ticket.role)))
  RETURNING coalesce(ticket.user_id,''),coalesce(ticket.browser_session_id,''),coalesce(ticket.terminal_session_id,''),coalesce(ticket.inspector_credential_id,''),ticket.origin,ticket.role,ticket.policy_generation,ticket.team_generation,ticket.membership_generation,ticket.binding_generation,ticket.browser_public_key_sha256,ticket.expires_at,coalesce(ticket.guest_session_id,'')`, hash[:], origin, terminalSessionID).Scan(&out.UserID, &out.BrowserSessionID, &out.TerminalSessionID, &out.InspectorCredentialID, &out.Origin, &out.Role, &out.PolicyGeneration, &out.TeamGeneration, &out.MembershipGeneration, &out.BindingGeneration, &out.BrowserPublicKeySHA256, &out.ExpiresAt, &out.GuestSessionID)
	if err == sql.ErrNoRows {
		return Ticket{}, ErrTicketInvalid
	}
	if err != nil {
		return Ticket{}, err
	}
	return out, nil
}

type ticketTransactionCleanupError struct {
	operation error
	cleanup   error
}

func (ticketTransactionCleanupError) Error() string {
	return "browser terminal ticket transaction cleanup failed"
}

func (e ticketTransactionCleanupError) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.operation != nil {
		causes = append(causes, e.operation)
	}
	if e.cleanup != nil {
		causes = append(causes, e.cleanup)
	}
	return causes
}

func (s TicketStore) SessionActive(ctx context.Context, userID, browserSessionID string) (bool, error) {
	if s.DB == nil || userID == "" || browserSessionID == "" {
		return false, nil
	}
	var active bool
	err := s.DB.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions auth_session JOIN users account ON account.id=auth_session.user_id
		WHERE auth_session.id=$1 AND auth_session.user_id=$2 AND auth_session.revoked_at IS NULL AND account.status='active')`, browserSessionID, userID).Scan(&active)
	return active, err
}

func validRole(role string) bool {
	return role == "owner" || role == "viewer" || role == "interactive" || role == RoleInspector
}

// validResource: a terminal ticket names its terminal session; an inspector
// ticket names one inspector credential and has an account principal.
func (t Ticket) validResource() bool {
	if t.Role == RoleInspector {
		return t.TerminalSessionID == "" && t.InspectorCredentialID != "" && t.GuestSessionID == ""
	}
	return t.TerminalSessionID != "" && t.InspectorCredentialID == ""
}

func validPublicKeySHA256(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

// Principal branches remain explicit; guests never participate in account auth.
func (t Ticket) validPrincipal() bool {
	return (t.GuestSessionID == "" && t.UserID != "" && t.BrowserSessionID != "") ||
		(t.GuestSessionID != "" && t.UserID == "" && t.BrowserSessionID == "" && (t.Role == "viewer" || t.Role == "interactive") && t.TeamGeneration == 0 && t.MembershipGeneration == 0 && t.BindingGeneration == 0)
}
func (t Ticket) subjectID() string {
	if t.GuestSessionID != "" {
		return t.GuestSessionID
	}
	return t.UserID
}
func (t Ticket) principalSessionID() string {
	if t.GuestSessionID != "" {
		return t.GuestSessionID
	}
	return t.BrowserSessionID
}
func (s TicketStore) GuestSessionActive(ctx context.Context, guestID string) (bool, error) {
	if s.DB == nil || guestID == "" {
		return false, nil
	}
	var active bool
	err := s.DB.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
 WHERE g.id=$1 AND c.enabled AND c.generation=g.code_generation)`, guestID).Scan(&active)
	return active, err
}
