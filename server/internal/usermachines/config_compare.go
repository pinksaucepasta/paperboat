package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"time"
)

type ConfigCompareDescriptor struct {
	ExecDescriptor
	Binding mint.ConfigCompareBinding `json:"binding"`
}

func (s *Service) ConfigCompareDescriptor(ctx context.Context, account, environment, source, cli string, binding mint.ConfigCompareBinding) (ConfigCompareDescriptor, error) {
	if s.controlSigner == nil || source == "" || cli == "" || binding.AssignmentID == "" {
		return ConfigCompareDescriptor{}, ErrNotFound
	}
	sourceRow, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: source, UserID: account})
	if err != nil || sourceRow.RevokedAt.Valid || sourceRow.DeletedAt.Valid {
		return ConfigCompareDescriptor{}, ErrNotFound
	}
	var machineID string
	err = s.db.SQL().QueryRowContext(ctx, `SELECT m.id FROM user_machines m JOIN control_config_assignments a ON a.machine_id=m.id WHERE m.environment_id=$1 AND m.user_id=$2 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND a.id=$3 AND a.version=$4`, environment, account, binding.AssignmentID, binding.AssignmentVersion).Scan(&machineID)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigCompareDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ConfigCompareDescriptor{}, err
	}
	row, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: account})
	if err != nil {
		return ConfigCompareDescriptor{}, err
	}
	if !row.Online || row.State != "online" {
		return ConfigCompareDescriptor{}, ErrMachineCapabilityUnavailable
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, environment)
	if err != nil {
		return ConfigCompareDescriptor{}, ErrMachineCapabilityUnavailable
	}
	httpIngressURL, err := s.runtimeIngressURL(ctx, environment, route.PublicHost)
	if err != nil {
		return ConfigCompareDescriptor{}, err
	}
	var cliActive bool
	if err = s.db.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cli_client_sessions WHERE id=$1 AND user_id=$2 AND state='active' AND revoked_at IS NULL)`, cli, account).Scan(&cliActive); err != nil {
		return ConfigCompareDescriptor{}, err
	}
	if !cliActive {
		return ConfigCompareDescriptor{}, ErrNotFound
	}
	now := s.now().UTC()
	expires := now.Add(2 * time.Minute)
	jti := newID("token")
	operationID := newID("operation")
	scopeID := "personal"
	if scope, ok := workspace.FromContext(ctx); ok {
		scopeID = scope.ID
	}
	token, err := s.controlSigner.SignCredential(mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: account, JTI: jti, IssuedAt: now, ExpiresAt: expires, CredentialClass: "config_compare", Scopes: []string{"config:compare"}, EnvironmentID: environment, AccountID: account, UserID: account, MachineID: machineID, InstallationGeneration: row.InstallationGeneration, SourceMachineID: source, CLIClientSessionID: cli, OperationID: operationID, AssignmentID: binding.AssignmentID, AssignmentVersion: binding.AssignmentVersion, ConfigPath: binding.ConfigPath, ConflictRevision: binding.ConflictRevision, ExpectedRemoteRevision: binding.ExpectedRemoteRevision})
	if err != nil {
		return ConfigCompareDescriptor{}, err
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return ConfigCompareDescriptor{}, err
	}
	defer tx.Rollback()
	var lockedAccount string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=$1 AND status='active' FOR UPDATE", account).Scan(&lockedAccount); err != nil {
		return ConfigCompareDescriptor{}, ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM config_compare_admissions WHERE expires_at<now()"); err != nil {
		return ConfigCompareDescriptor{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM config_compare_admissions WHERE account_id=$1 AND expires_at>now()`, account).Scan(&count); err != nil {
		return ConfigCompareDescriptor{}, err
	}
	if count >= 128 {
		return ConfigCompareDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO config_compare_admissions(id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,native_cli_session_id,source_machine_id,workspace_id,expires_at,operation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, jti, account, environment, machineID, row.InstallationGeneration, binding.AssignmentID, binding.AssignmentVersion, binding.ConfigPath, binding.ConflictRevision, binding.ExpectedRemoteRevision, cli, source, scopeID, expires, operationID); err != nil {
		return ConfigCompareDescriptor{}, err
	}
	if err = tx.Commit(); err != nil {
		return ConfigCompareDescriptor{}, err
	}
	return ConfigCompareDescriptor{ExecDescriptor: ExecDescriptor{OperationID: operationID, Environment: map[string]any{"id": environment, "kind": accessdescriptor.EnvironmentMachine, "resource_id": machineID, "alias": row.Alias, "state": "ready", "root": row.WorkspaceRoot}, Endpoints: machineTerminalEndpoints(runtimeWebsocketBase(httpIngressURL)), Auth: map[string]any{"method": "bearer", "token": token, "access_session_id": jti, "expires_at": expires, "scopes": []string{"config:compare"}}, ExpiresAt: expires}, Binding: binding}, nil
}
