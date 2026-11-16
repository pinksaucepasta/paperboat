package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/naming"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/sessionname"
)

// AdoptOfflineTerminalSession registers a terminal session the machine started while the client could not reach
// Paperboat (id terminal_offline_*, F21-11): once the machine is online the caller says the id, the host confirms it
// knows that session, and the session gets the row every other session has, so it is listed, can be closed and
// follows the same rules. Calling it again for an adopted session returns the row. The host's session name is kept
// unless the caller already uses it for another session on this machine.
func (s *Service) AdoptOfflineTerminalSession(ctx context.Context, userID, userMachineID, sessionID string) (adopted TerminalSession, adoptErr error) {
	step := "check the id"
	defer func() {
		if adoptErr != nil {
			adoptErr = fmt.Errorf("adopt offline terminal session: %s: %w", step, adoptErr)
		}
	}()
	if !sessionname.IsOfflineID(sessionID) {
		return TerminalSession{}, fmt.Errorf("%q is not the id of a session opened without Paperboat: %w", sessionID, ErrTerminalSessionNotFound)
	}
	step = "look the session up"
	lookup := dbsqlc.GetUserMachineTerminalSessionParams{ID: sessionID, UserMachineID: userMachineID, UserID: userID}
	if row, err := getTerminalSession(ctx, s.db.Queries(), lookup); err == nil {
		return mapTerminalSession(row), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TerminalSession{}, err
	}
	step = "check the machine"
	machine, err := s.machineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TerminalSession{}, ErrNotFound
		}
		return TerminalSession{}, err
	}
	if err := terminalHostAvailability(machine); err != nil {
		return TerminalSession{}, err
	}
	// The row comes first: the credential the host is asked with names a stored session. A session the host
	// does not know is removed again below.
	step = "register it"
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		scope, scoped := workspace.FromContext(ctx)
		if scoped {
			if err := workspace.ValidateTx(ctx, tx, userID, scope); err != nil {
				return err
			}
		}
		if _, err := lockTerminalSessions(ctx, tx.Queries(), dbsqlc.LockUserMachineTerminalSessionsParams{UserMachineID: userMachineID, UserID: userID}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		ownerTeamID := sql.NullString{}
		if scoped && scope.TeamID != "" {
			ownerTeamID = sql.NullString{String: scope.TeamID, Valid: true}
		} else if !scoped && machine.OwnerTeamID.Valid {
			ownerTeamID = machine.OwnerTeamID
		}
		for attempts := 0; attempts < 32; attempts++ {
			name, err := naming.Session(s.nameRandom)
			if err != nil {
				return err
			}
			created, err := tx.Queries().CreateUserMachineTerminalSession(ctx, dbsqlc.CreateUserMachineTerminalSessionParams{OwnerAccount: userID, OwnerTeamID: ownerTeamID, ID: sessionID, UserMachineID: userMachineID, Name: name, LaunchCwd: machine.WorkspaceRoot})
			if err != nil {
				return err
			}
			if created > 0 {
				return nil
			}
			if row, err := getTerminalSession(ctx, tx.Queries(), lookup); err == nil { // adopted by a concurrent call
				adopted = mapTerminalSession(row)
				return errAdoptedMeanwhile
			}
		}
		return ErrTerminalSessionConflict
	})
	if errors.Is(err, errAdoptedMeanwhile) {
		return adopted, nil
	}
	if err != nil {
		return TerminalSession{}, err
	}
	step = "ask the machine"
	snapshot, err := s.hostTerminalSnapshot(ctx, userID, userMachineID, sessionID)
	if err != nil {
		_, cleanupErr := s.db.SQL().ExecContext(context.WithoutCancel(ctx), `DELETE FROM user_machine_terminal_sessions WHERE id=$1 AND owner_account=$2 AND last_runtime_sync_at IS NULL`, sessionID, userID)
		return TerminalSession{}, errors.Join(err, cleanupErr)
	}
	step = "record what the machine knows"
	// The host's name wins unless the caller already uses it on this machine, then the generated one stays.
	if validUserMachineTerminalSessionName(snapshot.Name) {
		_, _ = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET name=$2 WHERE id=$1 AND NOT EXISTS (SELECT 1 FROM user_machine_terminal_sessions o WHERE o.user_machine_id=$3 AND o.owner_account=$4 AND lower(o.name)=lower($2) AND o.deleted_at IS NULL)`, sessionID, snapshot.Name, userMachineID, userID)
	}
	cwd := snapshot.CWD
	if cwd == "" {
		cwd = machine.WorkspaceRoot
	}
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET launch_cwd=$2,runtime_state=$3,last_runtime_sync_at=now(),updated_at=now() WHERE id=$1`, sessionID, cwd, snapshot.State); err != nil {
		return TerminalSession{}, err
	}
	step = "read it back"
	row, err := getTerminalSession(ctx, s.db.Queries(), lookup)
	if err != nil {
		return TerminalSession{}, err
	}
	return mapTerminalSession(row), nil
}

var errAdoptedMeanwhile = errors.New("session adopted by a concurrent call")

// hostTerminalSnapshot asks the machine's terminal host for one session as the account that owns it.
func (s *Service) hostTerminalSnapshot(ctx context.Context, userID, userMachineID, sessionID string) (helperruntime.Snapshot, error) {
	host := s.terminalHostRoute(ctx, userMachineID)
	if host.ingress == "" {
		return helperruntime.Snapshot{}, ErrMachineOffline
	}
	now := s.now().UTC()
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: host.environmentID, MachineID: userMachineID, UserID: userID, CLIClientSessionID: "control-plane", SessionID: sessionID})
	if err != nil {
		return helperruntime.Snapshot{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*terminalProbeTimeout)
	defer cancel()
	snapshot, err := s.controlRuntime.Terminal(probeCtx, host.ingress, token, "snapshot", sessionID, newID("operation"))
	switch {
	case err == nil:
		return snapshot, nil
	case helperruntime.IsSessionUnknown(err):
		return helperruntime.Snapshot{}, fmt.Errorf("the machine does not have session %s: %w", sessionID, ErrTerminalSessionNotFound)
	default:
		return helperruntime.Snapshot{}, errors.Join(ErrMachineOffline, err)
	}
}
