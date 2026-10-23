package usermachines

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

const TerminalCodeAttemptsPerMinute = 120
const TerminalCodeMaximumLifetime = 24 * time.Hour

var ErrTerminalCodeInvalid = errors.New("terminal code is invalid or unavailable")
var ErrTerminalCodeRateLimited = errors.New("terminal code join rate exceeded")
var ErrTerminalGuestCapacity = errors.New("terminal guest session capacity reached")

type TerminalCodeGuest struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type TerminalCodeAccess struct {
	TerminalSessionID string              `json:"terminal_session_id"`
	Enabled           bool                `json:"enabled"`
	Generation        uint64              `json:"generation"`
	Role              TerminalBrowserRole `json:"role"`
	ExpiresAt         *time.Time          `json:"expires_at,omitempty"`
	Code              string              `json:"code,omitempty"`
	Guests            []TerminalCodeGuest `json:"guests"`
}
type TerminalCodeAccessMutation struct {
	ExpectedGeneration       uint64              `json:"expected_generation"`
	Enabled                  bool                `json:"enabled"`
	Role                     TerminalBrowserRole `json:"role"`
	ExpiresInSeconds         int64               `json:"expires_in_seconds"`
	Reset                    bool                `json:"reset"`
	ConfirmInteractiveAccess bool                `json:"confirm_interactive_access"`
}
type TerminalGuestSession struct {
	ID        string
	Token     string
	SessionID string
	ExpiresAt time.Time
}

func (s *Service) TerminalCodeAccess(ctx context.Context, accountID, sessionID string) (TerminalCodeAccess, error) {
	out := TerminalCodeAccess{TerminalSessionID: sessionID, Role: TerminalBrowserRoleViewer}
	err := s.db.Pool().QueryRow(ctx, `SELECT coalesce(c.enabled,false),coalesce(c.generation,0),coalesce(c.role,'viewer'),c.expires_at
 FROM user_machine_terminal_sessions t LEFT JOIN terminal_code_access c ON c.terminal_session_id=t.id
 WHERE t.id=$1 AND t.owner_account=$2 AND t.deleted_at IS NULL`, sessionID, accountID).Scan(&out.Enabled, &out.Generation, &out.Role, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalCodeAccess{}, ErrTerminalSessionNotFound
	}
	if out.ExpiresAt != nil && !out.ExpiresAt.After(s.now()) {
		out.Enabled = false
	}
	out.Guests = []TerminalCodeGuest{}
	if err != nil {
		return TerminalCodeAccess{}, err
	}
	if out.Enabled {
		rows, err := s.db.Pool().Query(ctx, `SELECT g.id,g.display_name,g.expires_at FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
    WHERE g.terminal_session_id=$1 AND g.expires_at>now() AND c.enabled AND c.expires_at>now() AND c.generation=g.code_generation ORDER BY g.created_at LIMIT $2`, sessionID, planlimits.ViewersPerSharedTerminal())
		if err != nil {
			return TerminalCodeAccess{}, err
		}
		for rows.Next() {
			var guest TerminalCodeGuest
			if err := rows.Scan(&guest.ID, &guest.DisplayName, &guest.ExpiresAt); err != nil {
				rows.Close()
				return TerminalCodeAccess{}, err
			}
			out.Guests = append(out.Guests, guest)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return TerminalCodeAccess{}, err
		}
		rows.Close()
	}
	return out, nil
}
func (s *Service) SetTerminalCodeAccess(ctx context.Context, accountID, sessionID string, in TerminalCodeAccessMutation) (TerminalCodeAccess, error) {
	if in.Role != TerminalBrowserRoleViewer && in.Role != TerminalBrowserRoleInteractive {
		return TerminalCodeAccess{}, ErrTerminalBrowserPolicyInvalid
	}
	if in.Enabled && in.ExpiresInSeconds != 3600 && in.ExpiresInSeconds != 28800 && in.ExpiresInSeconds != 86400 {
		return TerminalCodeAccess{}, ErrTerminalBrowserPolicyInvalid
	}
	var out TerminalCodeAccess
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var owner, state, runtime string
		err := tx.QueryRow(ctx, `SELECT owner_account,desired_state,runtime_state FROM user_machine_terminal_sessions WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, sessionID).Scan(&owner, &state, &runtime)
		if errors.Is(err, pgx.ErrNoRows) || owner != accountID {
			return ErrTerminalSessionNotFound
		}
		if err != nil {
			return err
		}
		if in.Enabled && (state != "open" || runtime == "closed" || runtime == "exited" || runtime == "deleted") {
			return ErrTerminalBrowserSessionClosed
		}
		out = TerminalCodeAccess{TerminalSessionID: sessionID, Role: TerminalBrowserRoleViewer}
		err = tx.QueryRow(ctx, `SELECT enabled,generation,role,expires_at FROM terminal_code_access WHERE terminal_session_id=$1 FOR UPDATE`, sessionID).Scan(&out.Enabled, &out.Generation, &out.Role, &out.ExpiresAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if out.Generation != in.ExpectedGeneration || out.Generation >= 9007199254740991 {
			return ErrTerminalBrowserPolicyConflict
		}
		if in.Enabled && in.Role == TerminalBrowserRoleInteractive && (!out.Enabled || out.Role != in.Role) && !in.ConfirmInteractiveAccess {
			return ErrTerminalBrowserInteractionConfirmationRequired
		}
		currentlyActive := out.Enabled && out.ExpiresAt != nil && out.ExpiresAt.After(s.now())
		if (!in.Enabled && !out.Enabled) || (in.Enabled && currentlyActive && !in.Reset && in.Role == out.Role) {
			return nil
		}
		out.Generation++
		out.Enabled = in.Enabled
		out.Role = in.Role
		out.ExpiresAt = nil
		var hash []byte
		if in.Enabled {
			code, err := newTerminalJoinCode()
			if err != nil {
				return err
			}
			out.Code = code
			sum := terminalJoinCodeHash(sessionID, code)
			hash = sum[:]
			expiry := s.now().UTC().Add(time.Duration(in.ExpiresInSeconds) * time.Second)
			out.ExpiresAt = &expiry
		}
		_, err = tx.Exec(ctx, `INSERT INTO terminal_code_access(terminal_session_id,generation,enabled,code_hash,role,expires_at)
  VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(terminal_session_id) DO UPDATE SET generation=excluded.generation,enabled=excluded.enabled,code_hash=excluded.code_hash,role=excluded.role,expires_at=excluded.expires_at`, sessionID, out.Generation, out.Enabled, hash, out.Role, out.ExpiresAt)
		if err != nil {
			return err
		}
		// All old guest identities (and their tickets) are invalid after rotation/stop.
		if _, err = tx.Exec(ctx, `DELETE FROM terminal_guest_sessions WHERE terminal_session_id=$1`, sessionID); err != nil {
			return err
		}
		if s.audit != nil {
			return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: accountID, ActorType: audit.ActorUser, EventType: "terminal.code_access_changed", ResourceType: "terminal_session", ResourceID: sessionID, Metadata: map[string]any{"enabled": out.Enabled, "role": out.Role, "generation": out.Generation}})
		}
		return nil
	})
	out.Guests = []TerminalCodeGuest{}
	return out, err
}

// JoinTerminalWithCode creates a bounded, exact-terminal principal, never an account.
// The attempt reservation commits even on invalid code, across server processes.
func (s *Service) JoinTerminalWithCode(ctx context.Context, sessionID, code, displayName string) (TerminalGuestSession, error) {
	displayName = strings.TrimSpace(displayName)
	if !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) < 1 || utf8.RuneCountInString(displayName) > 64 || strings.IndexFunc(displayName, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return TerminalGuestSession{}, ErrTerminalCodeInvalid
	}
	var attempt int
	err := s.db.Pool().QueryRow(ctx, `UPDATE terminal_code_access SET
 attempts=CASE WHEN attempts_window<=now()-interval '1 minute' THEN 1 ELSE attempts+1 END,
 attempts_window=CASE WHEN attempts_window<=now()-interval '1 minute' THEN now() ELSE attempts_window END
 WHERE terminal_session_id=$1 AND (attempts_window<=now()-interval '1 minute' OR attempts<$2) RETURNING attempts`, sessionID, TerminalCodeAttemptsPerMinute).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A non-existent terminal and invalid code produce the same denial.
		var exists bool
		if e := s.db.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM terminal_code_access WHERE terminal_session_id=$1)`, sessionID).Scan(&exists); e != nil {
			return TerminalGuestSession{}, e
		}
		if exists {
			return TerminalGuestSession{}, ErrTerminalCodeRateLimited
		}
		return TerminalGuestSession{}, ErrTerminalCodeInvalid
	}
	if err != nil {
		return TerminalGuestSession{}, err
	}
	var guest TerminalGuestSession
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var hash []byte
		var generation uint64
		var expires time.Time
		err := tx.QueryRow(ctx, `SELECT c.code_hash,c.generation,c.expires_at FROM terminal_code_access c
  JOIN user_machine_terminal_sessions t ON t.id=c.terminal_session_id
  JOIN user_machines m ON m.id=t.user_machine_id AND m.deleted_at IS NULL AND m.revoked_at IS NULL
  JOIN users owner ON owner.id=t.owner_account AND owner.status='active'
  WHERE c.terminal_session_id=$1 AND c.enabled AND c.expires_at>now() AND t.deleted_at IS NULL AND t.desired_state='open' AND t.runtime_state NOT IN ('closed','exited','deleted') FOR UPDATE OF c`, sessionID).Scan(&hash, &generation, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTerminalCodeInvalid
		}
		if err != nil {
			return err
		}
		normalized, valid := normalizeTerminalJoinCode(code)
		sum := terminalJoinCodeHash(sessionID, normalized)
		if !valid || subtle.ConstantTimeCompare(hash, sum[:]) != 1 {
			return ErrTerminalCodeInvalid
		}
		var count int
		// Only live viewers count: an expired guest frees its place.
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM terminal_guest_sessions WHERE terminal_session_id=$1 AND expires_at>now()`, sessionID).Scan(&count); err != nil {
			return err
		}
		if count >= planlimits.ViewersPerSharedTerminal() {
			return ErrTerminalGuestCapacity
		}
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			return err
		}
		guest.Token = base64.RawURLEncoding.EncodeToString(secret[:])
		tokenHash := sha256.Sum256(secret[:])
		guest.ID = newID("session")
		guest.SessionID = sessionID
		guest.ExpiresAt = expires
		_, err = tx.Exec(ctx, `INSERT INTO terminal_guest_sessions(id,token_hash,terminal_session_id,code_generation,display_name,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, guest.ID, tokenHash[:], sessionID, generation, displayName, expires)
		return err
	})
	return guest, err
}

// DiscardTerminalGuest releases a newly admitted session when its caller cannot
// deliver the grant. The terminal binding prevents cleanup from affecting another resource.
func (s *Service) DiscardTerminalGuest(ctx context.Context, sessionID, guestID string) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM terminal_guest_sessions WHERE terminal_session_id=$1 AND id=$2`, sessionID, guestID)
	return err
}

func (s *Service) AuthenticateTerminalGuest(ctx context.Context, sessionID, token string) (TerminalGuestSession, error) {
	secret, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != token {
		return TerminalGuestSession{}, ErrTerminalCodeInvalid
	}
	sum := sha256.Sum256(secret)
	var guest TerminalGuestSession
	err = s.db.Pool().QueryRow(ctx, `SELECT g.id,g.terminal_session_id,g.expires_at FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
 WHERE g.token_hash=$1 AND g.terminal_session_id=$2 AND g.expires_at>now() AND c.enabled AND c.expires_at>now() AND c.generation=g.code_generation`, sum[:], sessionID).Scan(&guest.ID, &guest.SessionID, &guest.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalGuestSession{}, ErrTerminalCodeInvalid
	}
	return guest, err
}
func (s *Service) ResolveTerminalGuestRole(ctx context.Context, guestID, sessionID string) (TerminalBrowserDecision, error) {
	var decision TerminalBrowserDecision
	decision.GuestSessionID = guestID
	err := s.db.Pool().QueryRow(ctx, `SELECT t.id,t.owner_account,coalesce(t.owner_team_id,'personal'),m.id,m.environment_id,c.role,c.generation
 FROM terminal_guest_sessions g JOIN terminal_code_access c ON c.terminal_session_id=g.terminal_session_id
 JOIN user_machine_terminal_sessions t ON t.id=g.terminal_session_id
 JOIN user_machines m ON m.id=t.user_machine_id AND m.deleted_at IS NULL AND m.revoked_at IS NULL
 JOIN users owner ON owner.id=t.owner_account AND owner.status='active'
 WHERE g.id=$1 AND g.terminal_session_id=$2 AND c.enabled AND c.generation=g.code_generation
 AND t.deleted_at IS NULL AND t.desired_state='open' AND t.runtime_state NOT IN ('closed','exited','deleted')`, guestID, sessionID).Scan(&decision.SessionID, &decision.OwnerAccountID, &decision.WorkspaceID, &decision.MachineID, &decision.EnvironmentID, &decision.Role, &decision.PolicyGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalBrowserDecision{}, ErrTerminalSessionNotFound
	}
	if err != nil {
		return TerminalBrowserDecision{}, err
	}
	return s.completeTerminalBrowserRoute(ctx, decision)
}

// TerminalBrowserSession reads metadata after an exact current grant; guests receive no inventory.
func (s *Service) TerminalBrowserSession(ctx context.Context, accountID, guestID, sessionID string) (SharedTerminalSession, error) {
	var decision TerminalBrowserDecision
	var err error
	if guestID != "" {
		decision, err = s.ResolveTerminalGuestRole(ctx, guestID, sessionID)
	} else {
		decision, err = s.ResolveTerminalBrowserRole(ctx, accountID, sessionID)
	}
	if err != nil {
		return SharedTerminalSession{}, err
	}
	var out SharedTerminalSession
	out.DesiredState = "open"
	out.ID = sessionID
	out.OwnerAccount = decision.OwnerAccountID
	out.Role = string(decision.Role)
	out.CanManage = decision.Role == TerminalBrowserRoleOwner
	out.BrowserOnly = !out.CanManage
	out.Target = SharedTerminalTarget{Kind: "machine", ID: decision.MachineID, MachineID: decision.MachineID, EnvironmentID: decision.EnvironmentID}
	out.Sharing.RecentOutputWarning = terminalRecentOutputWarning
	out.Sharing.Grants = []SharedTerminalGrant{}
	out.Participants = []SessionParticipant{}
	err = s.db.Pool().QueryRow(ctx, `SELECT t.name,t.runtime_state,m.alias,t.launch_cwd FROM user_machine_terminal_sessions t JOIN user_machines m ON m.id=t.user_machine_id WHERE t.id=$1`, sessionID).Scan(&out.Name, &out.State, &out.Target.Name, &out.StartedIn)
	if err != nil {
		return SharedTerminalSession{}, err
	}
	out.RuntimeState = out.State
	out.Machine = cleanTerminalMetadata(out.Target.Name, 128)
	out.StartedIn = cleanTerminalMetadata(out.StartedIn, 1024)
	current, err := s.resolveTerminalBrowserDecision(ctx, decision)
	if err != nil || !sameTerminalBrowserDecision(decision, current) {
		return SharedTerminalSession{}, ErrTerminalSessionNotFound
	}
	if observed, observeErr := s.SnapshotTerminalForBrowser(ctx, current); observeErr == nil {
		projectTerminalIdentification(&out, observed.Snapshot)
	} else {
		supportreport.CaptureFromContext(ctx, "paperboat-server", "http_terminal", "service_error", "terminal_host_unavailable", observeErr)
	}
	return out, nil
}

const terminalCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ" // Ambiguous characters excluded; generated without modulo bias.
func newTerminalJoinCode() (string, error) {
	value, err := randomCode(12)
	if err != nil {
		return "", err
	}
	return value[:4] + "-" + value[4:8] + "-" + value[8:], nil
}

func normalizeTerminalJoinCode(value string) (string, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "")
	if len(value) != 12 {
		return value, false
	}
	for _, char := range value {
		if !strings.ContainsRune(terminalCodeAlphabet, char) {
			return value, false
		}
	}
	return value, true
}
func terminalJoinCodeHash(sessionID, code string) [32]byte {
	normalized, _ := normalizeTerminalJoinCode(code)
	return sha256.Sum256([]byte("paperboat.terminal-code.v1\x00" + sessionID + "\x00" + normalized))
}
