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
	// terminalReprobeAfter is how long the host's last answer about a live session stands on an online machine
	// before the sweep asks again. It is also what re-checks every session of a machine that was offline and came
	// back: their last answer is older than this, so the next sweep asks the host (Kubernetes-style level-triggered
	// reconcile; the host is the source of truth).
	terminalReprobeAfter = 15 * time.Minute
	// terminalUnreachableAfter closes a live session the host has not confirmed for this long (the machine never
	// came back); it matches the closed-record retention, so the row is then removed 30 days later.
	terminalUnreachableAfter = 30 * 24 * time.Hour
	terminalUnreachableBatch = 100
)

// Why a session ended, shown by the console. Written once, with the close.
const (
	terminalClosedHostGone    = "host_gone"           // the host answered "unknown session"
	terminalClosedUnreachable = "unreachable_timeout" // the host did not confirm it for terminalUnreachableAfter
)

// terminalSnapshotClosedReasonSQL is the closed_reason a host snapshot ($2 = its state) leaves: a session already
// closed keeps its reason, a live state clears it. Used where $2 is the snapshot state.
const terminalSnapshotClosedReasonSQL = `CASE WHEN $2 IN ('closed','exited','deleted') THEN coalesce(closed_reason,CASE WHEN $2='exited' THEN 'exited' ELSE 'host_closed' END) ELSE NULL END`

// terminalLiveStatePredicate is the one definition of "a live terminal session" (it counts toward nothing but
// the sweep and the detail views). Columns are unqualified: it runs against user_machine_terminal_sessions.
const terminalLiveStatePredicate = `deleted_at IS NULL AND (
    (desired_state='open' AND runtime_state NOT IN ('closed','exited','deleted'))
    OR runtime_state IN ('creating','running','restarting','closing'))`

// markTerminalSessionAbsent records that the host no longer has the session: it stops counting and is shown closed.
func (s *Service) markTerminalSessionAbsent(ctx context.Context, sessionID string) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state='closed',closed_at=coalesce(closed_at,now()),closed_reason=coalesce(closed_reason,'host_gone'),last_runtime_sync_at=now(),updated_at=now()
 WHERE id=$1 AND runtime_state NOT IN ('closed','exited','deleted')`, sessionID)
	return err
}

// markConfirmedTerminalSessionAbsent is markTerminalSessionAbsent for a session the host has confirmed
// before. Right after create the host does not have the session yet and answers "unknown"; that must
// not close it. Sessions the host never confirmed are left to reconcileOrphanTerminalSessions.
// It reports whether it closed the row.
func (s *Service) markConfirmedTerminalSessionAbsent(ctx context.Context, sessionID string) (bool, error) {
	result, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state='closed',closed_at=coalesce(closed_at,now()),closed_reason=coalesce(closed_reason,'host_gone'),last_runtime_sync_at=now(),updated_at=now()
 WHERE id=$1 AND runtime_state NOT IN ('closed','exited','deleted') AND last_runtime_sync_at IS NOT NULL`, sessionID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

type terminalOrphan struct {
	sessionID, userMachineID, ownerAccount string
}

type terminalHost struct {
	environmentID, ingress string
}

// closeUnreachableTerminalSessions closes live sessions the host has not confirmed for terminalUnreachableAfter,
// whether the machine stayed offline or never answered: nothing is left to ask, and the row must not show
// "Unavailable" forever. A session never confirmed counts from its creation.
func (s *Service) closeUnreachableTerminalSessions(ctx context.Context) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state='closed',closed_at=coalesce(closed_at,now()),closed_reason=$3,updated_at=now()
 WHERE id IN (SELECT id FROM user_machine_terminal_sessions
  WHERE `+terminalLiveStatePredicate+` AND coalesce(last_runtime_sync_at,created_at) < $1
  ORDER BY coalesce(last_runtime_sync_at,created_at),id LIMIT $2 FOR UPDATE SKIP LOCKED)`,
		s.now().UTC().Add(-terminalUnreachableAfter), terminalUnreachableBatch, terminalClosedUnreachable)
	return err
}

// reconcileTerminalSessions asks the host about live sessions on online machines whose last answer is missing
// (a create the host refused or lost) or older than terminalReprobeAfter (including every session of a machine
// that just came back online), and closes the ones that have outlived terminalUnreachableAfter. Only the host's own
// "session unknown" answer closes a probed row. A host that cannot be reached changes nothing, and a host that
// answers a snapshot records its state, so a session the host closed or that exited ends here too.
func (s *Service) reconcileTerminalSessions(ctx context.Context) error {
	if s.controlSigner == nil || s.controlRuntime == nil {
		return nil
	}
	return errors.Join(s.closeUnreachableTerminalSessions(ctx), s.probeStaleTerminalSessions(ctx))
}

func (s *Service) probeStaleTerminalSessions(ctx context.Context) error {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id,user_machine_id,owner_account FROM user_machine_terminal_sessions
 WHERE `+terminalLiveStatePredicate+` AND created_at < $1 AND (last_runtime_sync_at IS NULL OR last_runtime_sync_at < $2)
   AND EXISTS(SELECT 1 FROM user_machines m WHERE m.id=user_machine_id AND m.online AND m.deleted_at IS NULL)
 ORDER BY updated_at,id LIMIT $3`,
		s.now().UTC().Add(-terminalOrphanBound), s.now().UTC().Add(-terminalReprobeAfter), terminalOrphanBatch)
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
// is recorded. Any other host outcome changes nothing; only signing and database errors are returned.
func (s *Service) checkTerminalSession(ctx context.Context, o terminalOrphan, host terminalHost) error {
	now := s.now().UTC()
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: o.ownerAccount, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: host.environmentID, MachineID: o.userMachineID, UserID: o.ownerAccount, CLIClientSessionID: "control-plane", SessionID: o.sessionID})
	if err != nil {
		return err
	}
	snapshot, observeErr := s.controlRuntime.Terminal(ctx, host.ingress, token, "snapshot", o.sessionID, newID("operation"))
	switch {
	case observeErr == nil:
		_, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state=$2,closed_at=CASE WHEN $2 IN ('closed','exited','deleted') THEN coalesce(closed_at,now()) ELSE NULL END,closed_reason=`+terminalSnapshotClosedReasonSQL+`,last_runtime_sync_at=now(),updated_at=now() WHERE id=$1`, o.sessionID, snapshot.State)
		return err
	case helperruntime.IsSessionUnknown(observeErr):
		return s.markTerminalSessionAbsent(ctx, o.sessionID)
	default:
		// Move the row to the back of the queue: a host that does not answer must not hold the batch
		// for the orphans behind it.
		_, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET updated_at=now() WHERE id=$1`, o.sessionID)
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
