package browserterminal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"math"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var ErrTicketInvalid = errors.New("browser terminal ticket is invalid or expired")

// ErrSessionInactive means the browser sign-in that asked for a ticket has ended or been revoked.
var ErrSessionInactive = errors.New("browser terminal sign-in is no longer active")

// TooManyPendingTicketsError means the browser session already holds the most unredeemed tickets it may. A ticket
// that is not redeemed lapses after TicketLifetime, so the caller can retry after RetryAfter.
type TooManyPendingTicketsError struct{ RetryAfter time.Duration }

func (e TooManyPendingTicketsError) Error() string {
	return "too many unredeemed browser terminal tickets; retry in " + e.RetryAfter.String()
}

const TicketLifetime = 30 * time.Second

// maxPendingTickets bounds the unredeemed tickets one browser session may hold.
const maxPendingTickets = 8

type Ticket struct {
	Value                  string
	GuestSessionID         string
	UserID                 string
	BrowserSessionID       string
	TerminalSessionID      string
	InspectorCredentialID  string
	RecordingID            string
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
	// A consumed edge ticket is kept while its attachment is attached (and an hour after), so the
	// edge can resume that attachment's control channel; then it goes like any other.
	_, err := s.DB.SQL().ExecContext(ctx, `DELETE FROM browser_terminal_tickets ticket WHERE ticket.expires_at < now() - interval '1 hour'
		AND NOT EXISTS (SELECT 1 FROM native_usage_authorities usage WHERE usage.access_session_id=ticket.edge_attachment_id
			AND usage.resource_kind='terminal' AND usage.attached_until > now() - interval '1 hour')`)
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
			return Ticket{}, ErrSessionInactive
		}
		if err != nil {
			return Ticket{}, err
		}
		if !active {
			return Ticket{}, ErrSessionInactive
		}
	}
	var pending int
	var retryAfterSeconds float64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(extract(epoch FROM min(expires_at) - now()), 0)::float8 FROM browser_terminal_tickets WHERE (browser_session_id=$1 OR guest_session_id=$1) AND consumed_at IS NULL AND expires_at>now()`, in.principalSessionID()).Scan(&pending, &retryAfterSeconds); err != nil {
		return Ticket{}, err
	}
	if pending >= maxPendingTickets {
		return Ticket{}, TooManyPendingTicketsError{RetryAfter: max(time.Second, time.Duration(math.Ceil(retryAfterSeconds))*time.Second)}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO browser_terminal_tickets (token_hash,user_id,browser_session_id,terminal_session_id,origin,role,policy_generation,team_generation,membership_generation,binding_generation,browser_public_key_sha256,expires_at,guest_session_id,inspector_credential_id,recording_id)
		VALUES ($1,nullif($2,''),nullif($3,''),nullif($4,''),$5,$6,$7,$8,$9,$10,$11,$12,nullif($13,''),nullif($14,''),nullif($15,''))`, hash[:], in.UserID, in.BrowserSessionID, in.TerminalSessionID, in.Origin, in.Role, in.PolicyGeneration, in.TeamGeneration, in.MembershipGeneration, in.BindingGeneration, in.BrowserPublicKeySHA256, in.ExpiresAt, in.GuestSessionID, in.InspectorCredentialID, in.RecordingID); err != nil {
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
	return s.consume(ctx, value, origin, terminalSessionID, "")
}

// ConsumeEdge resolves the terminal from the one-use secret. The edge need not
// learn or supply a resource identifier before the control plane checks it.
// It records the attachment the ticket admits, the binding a resume is checked against.
func (s TicketStore) ConsumeEdge(ctx context.Context, value, origin, attachmentID string) (Ticket, error) {
	if attachmentID == "" {
		return Ticket{}, ErrTicketInvalid
	}
	return s.consume(ctx, value, origin, "", attachmentID)
}

// ResumeEdge returns the ticket that admitted attachmentID, for an edge whose control channel was
// cut without an answer. The principal's session and account (or guest code) are checked again as at
// consumption; the caller re-runs the rest of admission.
func (s TicketStore) ResumeEdge(ctx context.Context, value, origin, attachmentID string) (Ticket, error) {
	hash, ok := ticketHash(value)
	if s.DB == nil || origin == "" || attachmentID == "" || !ok {
		return Ticket{}, ErrTicketInvalid
	}
	var out Ticket
	err := s.DB.SQL().QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM browser_terminal_tickets ticket
		WHERE ticket.token_hash=$1 AND ticket.origin=$2 AND ticket.edge_attachment_id=$3 AND ticket.consumed_at IS NOT NULL
		AND `+ticketPrincipalActive, hash[:], origin, attachmentID).Scan(out.scanTargets()...)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrTicketInvalid
	}
	if err != nil {
		return Ticket{}, err
	}
	return out, nil
}

func ticketHash(value string) ([sha256.Size]byte, bool) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != value {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(secret), true
}

// ticketPrincipalActive holds while the ticket's principal may still use it: the account's browser
// session and account, or the guest's session and its terminal's current access code.
const ticketPrincipalActive = `((ticket.guest_session_id IS NULL AND EXISTS(
   SELECT 1 FROM sessions auth_session JOIN users account ON account.id=auth_session.user_id
   WHERE auth_session.id=ticket.browser_session_id AND auth_session.user_id=ticket.user_id
   AND auth_session.revoked_at IS NULL AND auth_session.expires_at>now() AND account.status='active'))
   OR (ticket.guest_session_id IS NOT NULL AND EXISTS(
    SELECT 1 FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
    WHERE g.id=ticket.guest_session_id AND g.terminal_session_id=ticket.terminal_session_id AND g.expires_at>now()
    AND c.enabled AND c.expires_at>now() AND c.generation=g.code_generation AND c.generation=ticket.policy_generation AND c.role=ticket.role)))`

const ticketColumns = `coalesce(ticket.user_id,''),coalesce(ticket.browser_session_id,''),coalesce(ticket.terminal_session_id,''),coalesce(ticket.inspector_credential_id,''),ticket.origin,ticket.role,ticket.policy_generation,ticket.team_generation,ticket.membership_generation,ticket.binding_generation,ticket.browser_public_key_sha256,ticket.expires_at,coalesce(ticket.guest_session_id,''),coalesce(ticket.recording_id,'')`

func (t *Ticket) scanTargets() []any {
	return []any{&t.UserID, &t.BrowserSessionID, &t.TerminalSessionID, &t.InspectorCredentialID, &t.Origin, &t.Role, &t.PolicyGeneration, &t.TeamGeneration, &t.MembershipGeneration, &t.BindingGeneration, &t.BrowserPublicKeySHA256, &t.ExpiresAt, &t.GuestSessionID, &t.RecordingID}
}

func (s TicketStore) consume(ctx context.Context, value, origin, terminalSessionID, attachmentID string) (Ticket, error) {
	if s.DB == nil || origin == "" {
		return Ticket{}, ErrTicketInvalid
	}
	hash, ok := ticketHash(value)
	if !ok {
		return Ticket{}, ErrTicketInvalid
	}
	var out Ticket
	err := s.DB.SQL().QueryRowContext(ctx, `UPDATE browser_terminal_tickets ticket SET consumed_at=now(), edge_attachment_id=nullif($4,'')
		WHERE ticket.token_hash=$1 AND ticket.origin=$2 AND ($3='' OR ticket.terminal_session_id=$3)
		AND ticket.consumed_at IS NULL AND ticket.expires_at>now() AND `+ticketPrincipalActive+`
		RETURNING `+ticketColumns, hash[:], origin, terminalSessionID, attachmentID).Scan(out.scanTargets()...)
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
	return role == "owner" || role == "viewer" || role == "interactive" || role == RoleInspector || role == RoleRecording
}

// validResource: a terminal ticket names its terminal session; an inspector
// ticket names one inspector credential and a recording ticket one recording,
// both with an account principal.
func (t Ticket) validResource() bool {
	switch t.Role {
	case RoleInspector:
		return t.TerminalSessionID == "" && t.InspectorCredentialID != "" && t.GuestSessionID == "" && t.RecordingID == ""
	case RoleRecording:
		return t.TerminalSessionID == "" && t.InspectorCredentialID == "" && t.GuestSessionID == "" && t.RecordingID != ""
	}
	return t.TerminalSessionID != "" && t.InspectorCredentialID == "" && t.RecordingID == ""
}

// terminalLess reports whether a ticket's channel has no terminal session (inspector or recording replay).
func (t Ticket) terminalLess() bool { return t.Role == RoleInspector || t.Role == RoleRecording }

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
