package nativeprivateaccess

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat-server/internal/admission"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
)

func (r *SQLResolver) resolveMachineService(ctx context.Context, in Request) (target Target, returnErr error) {
	port, e := machineservices.ParseRoute(in.RouteID)
	if e != nil || in.Protocol != "tcp" {
		return Target{}, ErrInvalid
	}
	tx, e := r.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if e != nil {
		return Target{}, e
	}
	defer func() {
		returnErr = preserveNativeSQLTransactionCleanup(returnErr, tx.Rollback())
	}()
	authority, e := machineservices.ResolveTx(ctx, tx, in.UserID, in.ResourceID, port)
	if e != nil {
		if pureNativeRejection(e, machineservices.ErrDenied) {
			return Target{}, ErrDenied
		}
		return Target{}, e
	}
	now := r.now()
	out := Target{AccountID: in.AccountID, UserID: in.UserID, ResourceKind: "machine_service", ResourceID: in.ResourceID, MachineID: in.ResourceID, RouteID: in.RouteID, Protocol: "tcp", TargetScheme: "tcp", PolicyGeneration: authority.Generation}
	var services []byte
	var snapshotExpiry time.Time
	e = tx.QueryRowContext(ctx, `SELECT m.environment_id,s.installation_generation,s.boot_id,s.generation,s.services,s.expires_at FROM paperboat.user_machines m JOIN paperboat.machine_service_snapshots s ON s.machine_id=m.id AND s.installation_generation=m.installation_generation JOIN paperboat.control_environments env ON env.id=m.environment_id AND env.owner_user_id=m.user_id AND env.desired_state='active' AND env.revoked_at IS NULL WHERE m.id=$1 AND m.online AND m.state='online' AND s.expires_at>$2 AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=$3 AND c.user_id=$4 AND c.state='active' AND c.revoked_at IS NULL) FOR SHARE OF m,s`, in.ResourceID, now, in.CLIClientSessionID, in.UserID).Scan(&out.EnvironmentID, &out.InstallationGeneration, &out.BootID, &out.AnnouncementGeneration, &services, &snapshotExpiry)
	if e != nil {
		return Target{}, mapResolveError(e)
	}
	var ports []machineservices.Port
	if e = json.Unmarshal(services, &ports); e != nil {
		return Target{}, e
	}
	for _, p := range ports {
		if p.Port == port && machineservices.ValidPort(p) {
			out.TargetAddress = p.Address()
			break
		}
	}
	if out.TargetAddress == "" {
		return Target{}, ErrDenied
	}
	out.ResourceGeneration = out.InstallationGeneration
	out.RouteGeneration = out.PolicyGeneration
	out.TargetGeneration = out.AnnouncementGeneration
	// Reuse the exact capability-only session. Resolving current authority must not
	// extend its lifetime; issuance can create a replacement after expiration.
	e = tx.QueryRowContext(ctx, `SELECT id FROM paperboat.user_machine_access_sessions WHERE user_id=$1 AND user_machine_id=$2 AND cli_client_session_id=$3 AND machine_service_port=$4 AND machine_service_source_kind=$5 AND machine_service_source_id=$6 AND machine_service_source_generation=$7 AND team_id IS NOT DISTINCT FROM nullif($8,'') AND state='active' AND revoked_at IS NULL AND expires_at>$9 AND machine_service_team_fence=paperboat.machine_service_team_fence(user_id,user_machine_id,machine_service_source_kind,machine_service_source_id,team_id) ORDER BY created_at DESC LIMIT 1`, in.UserID, in.ResourceID, in.CLIClientSessionID, port, authority.SourceKind, authority.SourceID, authority.Generation, authority.TeamID, now).Scan(&out.AccessSessionID)
	if pureNativeRejection(e, sql.ErrNoRows) {
		out.AccessSessionID = "session_" + uuid.NewString()
		_, e = tx.ExecContext(ctx, `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,expires_at,team_id,capabilities,operation_id,machine_service_port,machine_service_source_kind,machine_service_source_id,machine_service_source_generation,machine_service_team_fence) VALUES('control_plane_signed',$1,$2,$3,$4,$5,'',$6,nullif($7,''),ARRAY['private_access'],$8,$9,$10,$11,$12,paperboat.machine_service_team_fence($3,$2,$10,$11,nullif($7,'')))`, out.AccessSessionID, in.ResourceID, in.UserID, out.EnvironmentID, in.CLIClientSessionID, admission.Deadline(now, r.AdmissionLifetime), authority.TeamID, in.OperationID, port, authority.SourceKind, authority.SourceID, authority.Generation)
	}
	if e != nil {
		return Target{}, e
	}
	if e = tx.Commit(); e != nil {
		return Target{}, e
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
	port, e := machineservices.ParseRoute(in.RouteID)
	if e != nil {
		return time.Time{}, ErrDenied
	}
	var current, online bool
	var expiry time.Time
	var services []byte
	e = r.database.QueryRowContext(ctx, `SELECT paperboat.machine_service_session_allowed(a.id),s.expires_at,s.services,m.online FROM paperboat.user_machine_access_sessions a JOIN paperboat.user_machines m ON m.id=a.user_machine_id JOIN paperboat.machine_service_snapshots s ON s.machine_id=m.id WHERE a.id=$1 AND a.user_id=$2 AND a.cli_client_session_id=$3 AND a.user_machine_id=$4 AND a.machine_service_port=$5 AND a.machine_service_source_generation=$6 AND m.environment_id=$7 AND m.installation_generation=$8 AND s.installation_generation=$8 AND s.boot_id=$9 AND s.generation=$10`, session, in.UserID, in.CLIClientSessionID, in.ResourceID, port, expected.PolicyGeneration, expected.EnvironmentID, expected.InstallationGeneration, expected.BootID, expected.AnnouncementGeneration).Scan(&current, &expiry, &services, &online)
	if pureNativeRejection(e, sql.ErrNoRows) {
		return time.Time{}, ErrDenied
	}
	if e != nil {
		return time.Time{}, e
	}
	if !current {
		return time.Time{}, ErrDenied
	}
	var ports []machineservices.Port
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
