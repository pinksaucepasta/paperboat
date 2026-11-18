// Package nativeprivateaccess issues the grants that let a signed-in CLI client reach one machine
// service privately over the native (peer-to-peer or relayed) path. Service.Issue resolves the exact
// account, resource, route and protocol through a Resolver, checks that policy allows the port and
// that the machine currently announces the service, and returns a signed Grant. It never creates
// access sessions or probes a target.
package nativeprivateaccess

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
)

// resolveMachineService authorizes one machine service. READ COMMITTED plus a row lock on the CLI session, not
// SERIALIZABLE: a reconnect storm of grants collided with the machine heartbeats on predicate locks (40001 and
// 53200 after the retries, 503 native_private_unavailable; same cause as metering.RecordRuntimeObservation,
// 263462f6e). Authority is re-checked at every use (machine_service_session_allowed compares the source
// generation and team fence), so a grant that read authority just before a revocation is refused at use.
// Deadlocks are still replayed.
func (r *SQLResolver) resolveMachineService(ctx context.Context, in Request) (Target, error) {
	release, err := r.sessions.enter(ctx, in.UserID+"\x00"+in.CLIClientSessionID)
	if err != nil {
		return Target{}, err
	}
	defer release()
	var out Target
	err = db.RetrySerializable(ctx, func() (e error) {
		out, e = r.resolveMachineServiceAttempt(ctx, in)
		return e
	})
	return out, err
}

func (r *SQLResolver) resolveMachineServiceAttempt(ctx context.Context, in Request) (target Target, returnErr error) {
	port, e := wire.ParseRoute(in.RouteID)
	if e != nil || in.Protocol != "tcp" {
		return Target{}, ErrInvalid
	}
	tx, e := r.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return Target{}, e
	}
	defer func() {
		returnErr = preserveNativeSQLTransactionCleanup(returnErr, tx.Rollback())
	}()
	// Grants of one CLI session queue here, so the existing-session lookup and the insert below cannot both miss
	// and issue two sessions, and the SDK operation count is read after the previous grant's insert.
	var locked string
	if e := tx.QueryRowContext(ctx, `SELECT id FROM paperboat.cli_client_sessions WHERE id=$1 AND user_id=$2 FOR NO KEY UPDATE`, in.CLIClientSessionID, in.UserID).Scan(&locked); e != nil {
		return Target{}, mapResolveError(e)
	}
	if e := auth.AuthorizeAPITokenSession(ctx, tx, in.UserID, in.CLIClientSessionID, in.ResourceID, "private_access", port); e != nil {
		if e == auth.ErrOutsideTokenScope {
			return Target{}, ErrDenied
		}
		return Target{}, e
	}
	if e := auth.AuthorizeRecipeSession(ctx, tx, in.UserID, in.CLIClientSessionID, in.ResourceID, "private_access", port); e != nil {
		if e == auth.ErrOutsideReach {
			return Target{}, ErrDenied
		}
		return Target{}, e
	}
	authority, e := machineservices.ResolveTx(ctx, tx, in.UserID, in.ResourceID, port)
	if e != nil {
		if pureNativeRejection(e, machineservices.ErrDenied) {
			if off, offErr := machineservices.TeamPrivateAccessOffTx(ctx, tx, in.UserID, in.ResourceID); offErr == nil && off {
				return Target{}, machineservices.ErrTeamPrivateAccessOff
			}
			return Target{}, ErrDenied
		}
		return Target{}, e
	}
	if _, token := auth.SDKSessionFromContext(ctx); token {
		scope, _ := workspace.FromContext(ctx)
		if authority.TeamID != scope.TeamID {
			return Target{}, ErrDenied
		}
	}
	now := r.now()
	out := Target{AccountID: in.AccountID, UserID: in.UserID, ResourceKind: "machine_service", ResourceID: in.ResourceID, MachineID: in.ResourceID, RouteID: in.RouteID, Protocol: "tcp", TargetScheme: "tcp", PolicyGeneration: authority.Generation}
	// No FOR SHARE: a share lock on rows the machine heartbeat rewrites would make the grant wait on every heartbeat.
	var services []byte
	var snapshotExpiry time.Time
	e = tx.QueryRowContext(ctx, `SELECT m.environment_id,s.installation_generation,s.boot_id,s.generation,s.services,s.expires_at FROM paperboat.user_machines m JOIN paperboat.machine_service_snapshots s ON s.machine_id=m.id AND s.installation_generation=m.installation_generation JOIN paperboat.control_environments env ON env.id=m.environment_id AND env.owner_user_id=m.user_id AND env.desired_state='active' AND env.revoked_at IS NULL WHERE m.id=$1 AND m.online AND m.state='online' AND s.expires_at>$2 AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=$3 AND c.user_id=$4 AND c.state='active' AND c.revoked_at IS NULL) `, in.ResourceID, now, in.CLIClientSessionID, in.UserID).Scan(&out.EnvironmentID, &out.InstallationGeneration, &out.BootID, &out.AnnouncementGeneration, &services, &snapshotExpiry)
	if e != nil {
		return Target{}, mapResolveError(e)
	}
	var ports []wire.Port
	if e = json.Unmarshal(services, &ports); e != nil {
		return Target{}, e
	}
	for _, p := range ports {
		if p.Port == port && wire.ValidPort(p) {
			out.TargetAddress = p.Address()
			break
		}
	}
	if out.TargetAddress == "" {
		return Target{}, ErrServiceNotAnnounced
	}
	out.ResourceGeneration = out.InstallationGeneration
	out.RouteGeneration = out.PolicyGeneration
	out.TargetGeneration = out.AnnouncementGeneration
	// Reuse the exact capability-only session. Resolving current authority must not
	// extend its lifetime; issuance can create a replacement after expiration.
	tokenOperation := ""
	if _, token := auth.SDKSessionFromContext(ctx); token {
		tokenOperation = in.OperationID
	}
	e = tx.QueryRowContext(ctx, `SELECT id FROM paperboat.user_machine_access_sessions WHERE user_id=$1 AND user_machine_id=$2 AND cli_client_session_id=$3 AND machine_service_port=$4 AND machine_service_source_kind=$5 AND machine_service_source_id=$6 AND machine_service_source_generation=$7 AND team_id IS NOT DISTINCT FROM nullif($8,'') AND state='active' AND revoked_at IS NULL AND expires_at>$9 AND ($10='' OR operation_id=$10) AND machine_service_team_fence=paperboat.machine_service_team_fence(user_id,user_machine_id,machine_service_source_kind,machine_service_source_id,team_id) ORDER BY created_at DESC LIMIT 1`, in.UserID, in.ResourceID, in.CLIClientSessionID, port, authority.SourceKind, authority.SourceID, authority.Generation, authority.TeamID, now, tokenOperation).Scan(&out.AccessSessionID)
	inserted := false
	if pureNativeRejection(e, sql.ErrNoRows) {
		if token, ok := auth.SDKSessionFromContext(ctx); ok {
			if err := planlimits.CheckSDKCount(ctx, func(ctx context.Context, q string, args ...any) planlimits.SDKRow {
				return tx.QueryRowContext(ctx, q, args...)
			}, in.UserID, token, "operations"); err != nil {
				return Target{}, err
			}
		}
		inserted = true
		out.AccessSessionID = "session_" + uuid.NewString()
		_, e = tx.ExecContext(ctx, `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,team_id,capabilities,operation_id,machine_service_port,machine_service_source_kind,machine_service_source_id,machine_service_source_generation,machine_service_team_fence) VALUES('control_plane_signed',$1,$2,$3,$4,$5,'',$6,nullif($7,''),ARRAY['private_access'],$8,$9,$10,$11,$12,paperboat.machine_service_team_fence($3,$2,$10,$11,nullif($7,'')))`, out.AccessSessionID, in.ResourceID, in.UserID, out.EnvironmentID, in.CLIClientSessionID, admission.Deadline(now, r.AdmissionLifetime), authority.TeamID, in.OperationID, port, authority.SourceKind, authority.SourceID, authority.Generation)
	}
	if e != nil {
		return Target{}, e
	}
	if e = tx.Commit(); e != nil {
		return Target{}, e
	}
	if inserted {
		// Published after the commit, not by a trigger inside the transaction: every session of one
		// machine bumps the same topic row (see usermachines.publishAccessSession).
		if _, e = r.database.ExecContext(ctx, `SELECT paperboat.publish_machine_topic($1::text[],'machine_access')`, "{"+out.MachineID+"}"); e != nil {
			return Target{}, e
		}
	}
	return out, nil
}

type nativeSQLTransactionCleanupFailure struct {
	operation error
	rollback  error
}

func (*nativeSQLTransactionCleanupFailure) Error() string {
	return "native private access transaction cleanup failed"
}

func (failure *nativeSQLTransactionCleanupFailure) Unwrap() []error {
	return []error{failure.operation, failure.rollback}
}

func preserveNativeSQLTransactionCleanup(operation, rollback error) error {
	if rollback == nil || isExactSQLTransactionDone(rollback) {
		return operation
	}
	return &nativeSQLTransactionCleanupFailure{operation: operation, rollback: rollback}
}

func isExactSQLTransactionDone(err error) bool {
	if reflect.TypeOf(err) != reflect.TypeOf(sql.ErrTxDone) {
		return false
	}
	return err == sql.ErrTxDone
}

// MachineServiceCurrent never creates access sessions or probes a target. The
// authenticated runtime presents the verified caller and exact signed binding.
func (r *SQLResolver) MachineServiceCurrent(ctx context.Context, in Request, session string, expected Target) (time.Time, error) {
	if in.UserID == "" || in.AccountID != in.UserID || in.ResourceKind != "machine_service" || session == "" {
		return time.Time{}, ErrDenied
	}
	port, e := wire.ParseRoute(in.RouteID)
	if e != nil {
		return time.Time{}, ErrDenied
	}
	// The announcement generation only has to be no older than the one the binding was issued under:
	// an unrelated service starting or stopping on the machine advances it, and must not end access
	// to a service that is still announced (the exact port is checked against the current set below).
	var current, online bool
	var expiry time.Time
	var services []byte
	e = r.database.QueryRowContext(ctx, `SELECT paperboat.machine_service_session_allowed(a.id),s.expires_at,s.services,m.online FROM paperboat.user_machine_access_sessions a JOIN paperboat.user_machines m ON m.id=a.user_machine_id JOIN paperboat.machine_service_snapshots s ON s.machine_id=m.id WHERE a.id=$1 AND a.user_id=$2 AND a.cli_client_session_id=$3 AND a.user_machine_id=$4 AND a.machine_service_port=$5 AND a.machine_service_source_generation=$6 AND m.environment_id=$7 AND m.installation_generation=$8 AND s.installation_generation=$8 AND s.boot_id=$9 AND s.generation>=$10`, session, in.UserID, in.CLIClientSessionID, in.ResourceID, port, expected.PolicyGeneration, expected.EnvironmentID, expected.InstallationGeneration, expected.BootID, expected.AnnouncementGeneration).Scan(&current, &expiry, &services, &online)
	if pureNativeRejection(e, sql.ErrNoRows) {
		return time.Time{}, ErrDenied
	}
	if e != nil {
		return time.Time{}, e
	}
	if !current {
		return time.Time{}, ErrDenied
	}
	var ports []wire.Port
	if e = json.Unmarshal(services, &ports); e != nil {
		return time.Time{}, e
	}
	found := false
	for _, p := range ports {
		if p.Port == port && p.Address() == expected.TargetAddress {
			found = true
		}
	}
	if !found || expected.ResourceID != in.ResourceID || expected.RouteID != "tcp:"+strconv.Itoa(port) || expected.ResourceGeneration != expected.InstallationGeneration || expected.RouteGeneration != expected.PolicyGeneration || expected.TargetGeneration != expected.AnnouncementGeneration || expected.Protocol != "tcp" || expected.TargetScheme != "tcp" {
		return time.Time{}, ErrDenied
	}

	if !online || !expiry.After(r.now()) {
		return time.Time{}, ErrAuthorityUnavailable
	}
	return expiry, nil
}
