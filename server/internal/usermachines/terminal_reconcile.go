package usermachines

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

const (
	terminalReconcileBudget      = 8 * time.Second
	terminalReconcileConcurrency = 4
	// terminalOrphanBound is how long a live session may go without the host confirming it before the sweep asks.
	// A create the host refused or lost never gets a snapshot, so this is what closes it.
	terminalOrphanBound = 2 * time.Minute
	terminalOrphanBatch = 32
)

// terminalLiveStatePredicate is the one definition of "a live terminal session" (it counts toward nothing but
// the sweep and the detail views). Columns are unqualified: it runs against user_machine_terminal_sessions.
const terminalLiveStatePredicate = `deleted_at IS NULL AND (
    (desired_state='open' AND runtime_state NOT IN ('closed','exited','deleted'))
    OR runtime_state IN ('creating','running','restarting','closing'))`

// markTerminalSessionAbsent records that the host no longer has the session: it stops counting and is shown closed.
func (s *Service) markTerminalSessionAbsent(ctx context.Context, sessionID string) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state='closed',closed_at=coalesce(closed_at,now()),last_runtime_sync_at=now(),updated_at=now()
 WHERE id=$1 AND runtime_state NOT IN ('closed','exited','deleted')`, sessionID)
	return err
}

type terminalOrphan struct {
	sessionID, userMachineID, ownerAccount string
}

type terminalHost struct {
	environmentID, ingress string
}

// reconcileOrphanTerminalSessions asks the host about live sessions it has never confirmed (a create the host
// refused or lost). Only the host's own "session unknown" answer closes a row. A host that cannot be reached
// changes nothing, and a host that answers a snapshot confirms the row so it is not asked again.
func (s *Service) reconcileOrphanTerminalSessions(ctx context.Context) error {
	if s.controlSigner == nil || s.controlRuntime == nil {
		return nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id,user_machine_id,owner_account FROM user_machine_terminal_sessions
 WHERE `+terminalLiveStatePredicate+` AND last_runtime_sync_at IS NULL AND created_at < $1
   AND EXISTS(SELECT 1 FROM user_machines m WHERE m.id=user_machine_id AND m.online AND m.deleted_at IS NULL)
 ORDER BY updated_at,id LIMIT $2`,
		s.now().UTC().Add(-terminalOrphanBound), terminalOrphanBatch)
	if err != nil {
		return err
	}
	var orphans []terminalOrphan
	for rows.Next() {
		var o terminalOrphan
		if err = rows.Scan(&o.sessionID, &o.userMachineID, &o.ownerAccount); err != nil {
			_ = rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(orphans) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, terminalReconcileBudget)
	defer cancel()
	hosts := map[string]terminalHost{}
	for _, o := range orphans {
		if _, seen := hosts[o.userMachineID]; !seen {
			hosts[o.userMachineID] = s.terminalHostRoute(ctx, o.userMachineID)
		}
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
		jobs = make(chan terminalOrphan)
	)
	for worker := 0; worker < terminalReconcileConcurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range jobs {
				if err := s.checkTerminalSession(ctx, o, hosts[o.userMachineID]); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for _, o := range orphans {
		if hosts[o.userMachineID].ingress == "" {
			continue
		}
		select {
		case jobs <- o:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return errors.Join(errs...)
}

// checkTerminalSession asks the host for one session. The host's "unknown" answer closes the row and its snapshot
// confirms it. Any other host outcome changes nothing; only signing and database errors are returned.
func (s *Service) checkTerminalSession(ctx context.Context, o terminalOrphan, host terminalHost) error {
	now := s.now().UTC()
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: o.ownerAccount, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: host.environmentID, MachineID: o.userMachineID, UserID: o.ownerAccount, CLIClientSessionID: "control-plane", SessionID: o.sessionID})
	if err != nil {
		return err
	}
	_, observeErr := s.controlRuntime.Terminal(ctx, host.ingress, token, "snapshot", o.sessionID, newID("operation"))
	switch {
	case observeErr == nil:
		_, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET last_runtime_sync_at=now() WHERE id=$1`, o.sessionID)
		return err
	case helperruntime.IsSessionUnknown(observeErr):
		return s.markTerminalSessionAbsent(ctx, o.sessionID)
	default:
		// Move the row to the back of the queue: a host that does not answer must not hold the batch
		// for the orphans behind it.
		_, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET updated_at=now() WHERE id=$1 AND last_runtime_sync_at IS NULL`, o.sessionID)
		return err
	}
}

// terminalHostRoute finds where the server reaches a machine's terminal host. A machine without an active route
// has an empty ingress and its sessions are left alone.
func (s *Service) terminalHostRoute(ctx context.Context, userMachineID string) terminalHost {
	var host terminalHost
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT environment_id FROM user_machines WHERE id=$1`, userMachineID).Scan(&host.environmentID); err != nil {
		return terminalHost{}
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, host.environmentID)
	if err != nil {
		return terminalHost{}
	}
	host.ingress, err = s.runtimeIngressURL(ctx, host.environmentID, route.PublicHost)
	if err != nil {
		return terminalHost{}
	}
	return host
}
