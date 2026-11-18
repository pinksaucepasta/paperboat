package operatoractions

import (
	"context"
	"github.com/jackc/pgx/v5"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"

	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

var (
	ErrForbidden = errors.New("operator action forbidden")
	ErrInvalid   = errors.New("invalid operator action")
	ErrNotFound  = errors.New("operator action target not found")
	ErrConflict  = errors.New("operator action conflict")
)

type Input struct {
	Revision int64 `json:"revision"`
	// Legal marks a publication_block as a legal takedown: the edge answers 451 instead of 403.
	Legal bool `json:"legal,omitempty"`
	// Operator management: operator_create takes Email and Role, operator_sessions_revoke a SessionID (or "all").
	Email     string `json:"email,omitempty"`
	Role      string `json:"role,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Plan grants: plan_grant takes ProductCode, Days and (team plans) Quantity seats.
	ProductCode string `json:"product_code,omitempty"`
	Days        int    `json:"days,omitempty"`
	Quantity    int    `json:"quantity,omitempty"`
}
type Result struct {
	ResourceID string `json:"resource_id"`
	State      string `json:"state"`
	Version    int64  `json:"version"`
	// SetupToken is the operator invitation's one-time token. It is returned once and never stored in the receipt,
	// so an exact replay returns the result without it: invite again (new key) to issue a fresh token.
	SetupToken string     `json:"setup_token,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Revoked    int64      `json:"revoked,omitempty"`
}
type Service struct {
	store       *db.DB
	audit       *audit.Writer
	mail        config.TransactionalMail
	mailEnabled bool
}

func NewService(store *db.DB) *Service { return &Service{store: store, audit: audit.NewWriter(store)} }

// ConfigureMail supplies the deployment's mail policy for owner notices (a takedown). Without it
// the audit event is still written and no mail is queued.
func (s *Service) ConfigureMail(policy config.TransactionalMail, enabled bool) {
	s.mail, s.mailEnabled = policy, enabled
}

func (s *Service) Execute(ctx context.Context, actorID, key, kind, target, reason string, input Input) (Result, error) {
	actorID, key, kind, target, reason = strings.TrimSpace(actorID), strings.TrimSpace(key), strings.TrimSpace(kind), strings.TrimSpace(target), strings.TrimSpace(reason)
	if s == nil || s.store == nil || actorID == "" || len(key) < 8 || len(key) > 256 || target == "" || len(target) > 256 || reason == "" || len(reason) > 512 || !validKind(kind) || !inputFits(kind, input) {
		return Result{}, ErrInvalid
	}
	request, _ := json.Marshal(struct {
		Kind, Target, Reason string
		Input                Input
	}{kind, target, reason, input})
	hash := sha256.Sum256(request)
	operationLock := sha256.Sum256([]byte(actorID + "\x00" + key))
	var result Result
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 714214))`, hex.EncodeToString(operationLock[:])); err != nil {
			return err
		}
		var role, status string
		if err := tx.QueryRow(ctx, `SELECT role,status FROM operator_accounts WHERE id=$1`, actorID).Scan(&role, &status); err != nil {
			if operatorNoRowsOnly(err) {
				return ErrForbidden
			}
			return err
		}
		if status != "active" || !allowed(role, kind) {
			return ErrForbidden
		}
		var oldHash, raw []byte
		err := tx.QueryRow(ctx, `SELECT request_hash,result FROM operator_actions WHERE actor_user_id=$1 AND idempotency_key=$2`, actorID, key).Scan(&oldHash, &raw)
		if err == nil {
			if !equalHash(oldHash, hash[:]) {
				return ErrConflict
			}
			return json.Unmarshal(raw, &result)
		}
		if !operatorNoRowsOnly(err) {
			return err
		}
		switch kind {
		case "account_ban", "account_unban":
			if err = accountAction(ctx, tx, actorID, target, kind == "account_ban", input.Revision, &result); err != nil {
				return err
			}
		case "plan_grant", "plan_revoke":
			if err = grantAction(ctx, tx, kind, target, input, &result); err != nil {
				return err
			}
		case "account_sessions_revoke":
			if err = sessionsRevokeAction(ctx, tx, target, input.Revision, &result); err != nil {
				return err
			}
		case "team_ban", "team_unban":
			generation, teamErr := teams.SetOperatorSuspensionTx(ctx, tx, actorID, target, kind == "team_ban", input.Revision)
			if teamErr != nil {
				return mapTeamError(teamErr)
			}
			state := "active"
			if kind == "team_ban" {
				state = "suspended"
			}
			result = Result{ResourceID: target, State: state, Version: generation}
		case "operator_create", "operator_disable", "operator_enable", "operator_mfa_reset", "operator_sessions_revoke":
			if err = staffAction(ctx, tx, actorID, kind, target, input, &result); err != nil {
				return err
			}
		case "relay_drain", "relay_resume":
			if err = relayAction(ctx, tx, target, kind == "relay_drain", input.Revision, &result); err != nil {
				return err
			}
		case "publication_block", "publication_unblock":
			if err = s.publicationAction(ctx, tx, actorID, target, reason, kind == "publication_block", input, &result); err != nil {
				return err
			}
		case "tunnel_disable", "tunnel_enable":
			state := tunnelv1.DesiredActive
			if kind == "tunnel_disable" {
				state = tunnelv1.DesiredPaused
			}
			record, transitionErr := tunnelv1.OperatorTransitionTx(ctx, tx, actorID, key, target, reason, input.Revision, state)
			if transitionErr != nil {
				return mapTunnelError(transitionErr)
			}
			result = Result{ResourceID: record.Tunnel.ID, State: record.Tunnel.DesiredState, Version: record.Tunnel.Generation}
		}
		receipt := result
		receipt.SetupToken = ""
		raw, _ = json.Marshal(receipt)
		if _, err = tx.Exec(ctx, `INSERT INTO operator_actions(actor_user_id,idempotency_key,kind,target,request_hash,result) VALUES($1,$2,$3,$4,$5,$6)`, actorID, key, kind, target, hash[:], raw); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "operator." + kind, ResourceType: resourceType(kind), ResourceID: auditResource(kind, target, result), IdempotencyKey: key, Metadata: auditMetadata(kind, reason, input, result)})
	})
	return result, err
}

// operatorExpectedErrorOnly keeps expected domain results separate from
// outages. A joined or incomplete error tree is never accepted as proof of a
// pure rejection.
func operatorExpectedErrorOnly(err error, expected ...error) bool {
	seen := make(map[error]struct{}, 8)
	visited := 0
	var walk func(error) bool
	walk = func(current error) bool {
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Comparable() {
			if _, exists := seen[current]; exists {
				return false
			}
			seen[current] = struct{}{}
		}
		visited++
		if visited > 8 {
			return false
		}
		for _, sentinel := range expected {
			if sameOperatorError(current, sentinel) {
				return true
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !walk(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return walk(wrapped.Unwrap())
		}
		return false
	}
	return walk(err)
}

func sameOperatorError(current, sentinel error) bool {
	if current == nil || sentinel == nil {
		return current == nil && sentinel == nil
	}
	left, right := reflect.ValueOf(current), reflect.ValueOf(sentinel)
	return left.Type() == right.Type() && left.Comparable() && left.Interface() == right.Interface()
}

func operatorNoRowsOnly(err error) bool {
	return operatorExpectedErrorOnly(err, pgx.ErrNoRows)
}

func accountAction(ctx context.Context, tx *db.Tx, actor, target string, ban bool, revision int64, out *Result) error {
	if ban && actor == target {
		return ErrForbidden
	}
	var status string
	var version int64
	if err := tx.QueryRow(ctx, `SELECT status,version FROM users WHERE id=$1 FOR UPDATE`, target).Scan(&status, &version); err != nil {
		if operatorNoRowsOnly(err) {
			return ErrNotFound
		}
		return err
	}
	if status != "active" && status != "suspended" {
		return ErrConflict
	}
	if revision != version {
		return ErrConflict
	}
	wanted := "active"
	if ban {
		wanted = "suspended"
	}
	if status != wanted {
		if _, err := tx.Exec(ctx, `UPDATE users SET status=$2,version=version+1,updated_at=now() WHERE id=$1`, target, wanted); err != nil {
			return err
		}
		version++
	}
	if ban {
		if err := revokeUserAuthority(ctx, tx, target, "account_suspended"); err != nil {
			return err
		}
	}
	*out = Result{ResourceID: target, State: wanted, Version: version}
	return nil
}

// revokeUserAuthority ends the account's sessions and tokens: browser sessions and access grants, CLI sessions with
// their tokens, personal API tokens, recipes, pending device grants and machine access sessions. The state changes are
// the ones the revoke triggers publish to hosts (migrations 202/203), so open SSH sessions end within seconds. Peer
// network certificates are not revoked here: a new one needs a live CLI session, and an endpoint that is already
// connected keeps its identity until the certificate expires (account deletion revokes them).
// A ban and an operator's session revoke share it; only the ban also changes the account's status.
func revokeUserAuthority(ctx context.Context, tx *db.Tx, target, reason string) error {
	for _, statement := range []struct {
		query      string
		withReason bool // the query's $2 is the revocation reason
	}{
		{`UPDATE sessions SET revoked_at=coalesce(revoked_at,now()),updated_at=now(),version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`, false},
		{`UPDATE browser_access_sessions SET revoked_at=coalesce(revoked_at,now()) WHERE (account_id=$1 OR owner_account_id=$1) AND revoked_at IS NULL`, false},
		{`UPDATE cli_client_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),revocation_reason=coalesce(revocation_reason,$2),version=version+1 WHERE user_id=$1 AND state='active'`, true},
		{`UPDATE cli_access_tokens t SET revoked_at=coalesce(t.revoked_at,now()) FROM cli_client_sessions s WHERE t.cli_client_session_id=s.id AND s.user_id=$1`, false},
		{`UPDATE cli_refresh_tokens t SET state='revoked',revoked_at=coalesce(t.revoked_at,now()) FROM cli_client_sessions s WHERE t.cli_client_session_id=s.id AND s.user_id=$1 AND t.state<>'revoked'`, false},
		{`UPDATE api_tokens SET revoked_at=now(),revoked_by=$2 WHERE account_id=$1 AND revoked_at IS NULL`, true},
		// A recipe mints new CLI sessions on its own, so an account that stays active must lose them too.
		{`UPDATE recipes SET revoked_at=now(),revoked_by=$2 WHERE account_id=$1 AND revoked_at IS NULL`, true},
		{`UPDATE device_grants SET state='denied',denied_at=coalesce(denied_at,now()),version=version+1 WHERE user_id=$1 AND state IN('pending','approved')`, false},
		{`UPDATE user_machine_access_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),revocation_reason=$2,updated_at=now() WHERE user_id=$1 AND state='active'`, true},
	} {
		args := []any{target}
		if statement.withReason {
			args = append(args, reason)
		}
		if _, err := tx.Exec(ctx, statement.query, args...); err != nil {
			return err
		}
	}
	return nil
}

// sessionsRevokeAction ends all of the account's sessions and tokens and leaves the account usable: it can sign
// in again at once. The revision is the account's version, so a page that read an older state is refused.
func sessionsRevokeAction(ctx context.Context, tx *db.Tx, target string, revision int64, out *Result) error {
	var status string
	var version int64
	if err := tx.QueryRow(ctx, `SELECT status,version FROM users WHERE id=$1 FOR UPDATE`, target).Scan(&status, &version); err != nil {
		if operatorNoRowsOnly(err) {
			return ErrNotFound
		}
		return err
	}
	if (status != "active" && status != "suspended") || revision != version {
		return ErrConflict
	}
	if err := revokeUserAuthority(ctx, tx, target, "operator_revoked"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET version=version+1,updated_at=now() WHERE id=$1`, target); err != nil {
		return err
	}
	*out = Result{ResourceID: target, State: status, Version: version + 1}
	return nil
}

func relayAction(ctx context.Context, tx *db.Tx, target string, drain bool, revision int64, out *Result) error {
	// Managed nodes have a configuration revision independent of runtime heartbeats.
	// All their operator transitions go through that owner, avoiding competing CAS paths.
	var managed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM managed_node_authority WHERE node_id=$1)`, target).Scan(&managed); err != nil {
		return err
	}
	if managed {
		return ErrConflict
	}

	var state string
	var version int64
	if err := tx.QueryRow(ctx, `SELECT state,version FROM control_tunnel_nodes WHERE id=$1 AND (roles @> ARRAY['relay']::text[] OR roles @> ARRAY['peer_relay']::text[]) FOR UPDATE`, target).Scan(&state, &version); err != nil {
		if operatorNoRowsOnly(err) {
			return ErrNotFound
		}
		return err
	}
	if revision != version {
		return ErrConflict
	}
	wanted := "registered"
	if drain {
		wanted = "draining"
	}
	if state != wanted {
		if drain && state != "ready" && state != "registered" {
			return ErrConflict
		}
		if !drain && state != "draining" {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes SET state=$2,ready=false,drain_deadline=CASE WHEN $2='draining' THEN now()+interval '30 seconds' ELSE NULL END,version=version+1,updated_at=now() WHERE id=$1`, target, wanted); err != nil {
			return err
		}
		version++
	}
	*out = Result{ResourceID: target, State: wanted, Version: version}
	return nil
}

func validKind(v string) bool {
	switch v {
	case "account_ban", "account_unban", "account_sessions_revoke", "plan_grant", "plan_revoke", "team_ban", "team_unban", "relay_drain", "relay_resume", "tunnel_disable", "tunnel_enable", "publication_block", "publication_unblock", "operator_create", "operator_disable", "operator_enable", "operator_mfa_reset", "operator_sessions_revoke":
		return true
	}
	return false
}
func allowed(role, kind string) bool {
	if isStaffKind(kind) {
		return role == "admin"
	}
	if kind == "account_ban" || kind == "account_unban" || kind == "account_sessions_revoke" || isGrantKind(kind) || kind == "team_ban" || kind == "team_unban" {
		return role == "admin"
	}
	return role == "admin" || role == "operations"
}
func resourceType(kind string) string {
	if isStaffKind(kind) {
		return "operator_account"
	}
	if strings.HasPrefix(kind, "publication_") {
		return "publication"
	}
	if strings.HasPrefix(kind, "account_") {
		return "account"
	}
	if isGrantKind(kind) {
		return "billing_workspace"
	}
	if strings.HasPrefix(kind, "team_") {
		return "team"
	}
	if strings.HasPrefix(kind, "relay_") {
		return "relay"
	}
	return "tunnel"
}
func mapTeamError(err error) error {
	switch {
	case operatorExpectedErrorOnly(err, teams.ErrForbidden):
		return ErrForbidden
	case operatorExpectedErrorOnly(err, teams.ErrNotFound):
		return ErrNotFound
	case operatorExpectedErrorOnly(err, teams.ErrConflict):
		return ErrConflict
	case operatorExpectedErrorOnly(err, teams.ErrInvalid):
		return ErrInvalid
	default:
		return err
	}
}
func mapTunnelError(err error) error {
	switch {
	case operatorExpectedErrorOnly(err, tunnelv1.ErrOperatorForbidden):
		return ErrForbidden
	case operatorExpectedErrorOnly(err, tunnelv1.ErrNotFound):
		return ErrNotFound
	case operatorExpectedErrorOnly(err, tunnelv1.ErrGenerationConflict, tunnelv1.ErrIdempotencyConflict, tunnelv1.ErrTerminalState):
		return ErrConflict
	case operatorExpectedErrorOnly(err, tunnelv1.ErrInvalidInput):
		return ErrInvalid
	default:
		return err
	}
}
func equalHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}
