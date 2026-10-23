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

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
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
}
type Result struct {
	ResourceID string `json:"resource_id"`
	State      string `json:"state"`
	Version    int64  `json:"version"`
}
type Service struct {
	store *db.DB
	audit *audit.Writer
}

func NewService(store *db.DB) *Service { return &Service{store: store, audit: audit.NewWriter(store)} }

func (s *Service) Execute(ctx context.Context, actorID, key, kind, target, reason string, input Input) (Result, error) {
	actorID, key, kind, target, reason = strings.TrimSpace(actorID), strings.TrimSpace(key), strings.TrimSpace(kind), strings.TrimSpace(target), strings.TrimSpace(reason)
	if s == nil || s.store == nil || actorID == "" || len(key) < 8 || len(key) > 256 || target == "" || len(target) > 256 || reason == "" || len(reason) > 512 || input.Revision < 1 || !validKind(kind) {
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
		case "relay_drain", "relay_resume":
			if err = relayAction(ctx, tx, target, kind == "relay_drain", input.Revision, &result); err != nil {
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
		raw, _ = json.Marshal(result)
		if _, err = tx.Exec(ctx, `INSERT INTO operator_actions(actor_user_id,idempotency_key,kind,target,request_hash,result) VALUES($1,$2,$3,$4,$5,$6)`, actorID, key, kind, target, hash[:], raw); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorAdmin, EventType: "operator." + kind, ResourceType: resourceType(kind), ResourceID: target, IdempotencyKey: key, Metadata: map[string]any{"reason": reason, "state": result.State, "version": result.Version}})
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
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=coalesce(revoked_at,now()),updated_at=now(),version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_client_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),revocation_reason=coalesce(revocation_reason,'account_suspended'),version=version+1 WHERE user_id=$1 AND state='active'`, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_access_tokens t SET revoked_at=coalesce(t.revoked_at,now()) FROM cli_client_sessions s WHERE t.cli_client_session_id=s.id AND s.user_id=$1`, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_refresh_tokens t SET state='revoked',revoked_at=coalesce(t.revoked_at,now()) FROM cli_client_sessions s WHERE t.cli_client_session_id=s.id AND s.user_id=$1 AND t.state<>'revoked'`, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE device_grants SET state='denied',denied_at=coalesce(denied_at,now()),version=version+1 WHERE user_id=$1 AND state IN('pending','approved')`, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_machine_access_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),revocation_reason='account_suspended',updated_at=now() WHERE user_id=$1 AND state='active'`, target); err != nil {
			return err
		}
	}
	*out = Result{ResourceID: target, State: wanted, Version: version}
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
	case "account_ban", "account_unban", "team_ban", "team_unban", "relay_drain", "relay_resume", "tunnel_disable", "tunnel_enable":
		return true
	}
	return false
}
func allowed(role, kind string) bool {
	if kind == "account_ban" || kind == "account_unban" || kind == "team_ban" || kind == "team_unban" {
		return role == "admin"
	}
	return role == "admin" || role == "operations"
}
func resourceType(kind string) string {
	if strings.HasPrefix(kind, "account_") {
		return "account"
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
