package usermachines

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

const (
	terminalReconcileBudget      = 8 * time.Second
	terminalReconcileConcurrency = 4
)

// terminalLiveRowsPredicate is the one definition of "counts toward the machine's terminal session limit".
const terminalLiveRowsPredicate = `user_machine_id=$1 AND deleted_at IS NULL AND (
    (desired_state='open' AND runtime_state NOT IN ('closed','exited','deleted'))
    OR runtime_state IN ('creating','running','restarting','closing'))`

// markTerminalSessionAbsent records that the host no longer has the session: it stops counting and is shown closed.
func (s *Service) markTerminalSessionAbsent(ctx context.Context, sessionID string) error {
	_, err := s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state='closed',closed_at=coalesce(closed_at,now()),last_runtime_sync_at=now(),updated_at=now()
 WHERE id=$1 AND runtime_state NOT IN ('closed','exited','deleted')`, sessionID)
	return err
}

// reconcileLiveTerminalSessions asks the host about every session the server counts as live and closes the ones
// the host does not have (a host reinstall, a crashed daemon, an aborted run). A host that cannot be reached
// changes nothing: only the host's own "no such session" answer closes a row. It returns how many it closed.
func (s *Service) reconcileLiveTerminalSessions(ctx context.Context, userID, userMachineID string) (int, error) {
	if s.controlSigner == nil || s.controlRuntime == nil {
		return 0, nil
	}
	machine, err := s.Get(ctx, userID, userMachineID)
	if err != nil {
		return 0, err
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, machine.EnvironmentID)
	if err != nil {
		return 0, nil
	}
	ingress, err := s.runtimeIngressURL(ctx, machine.EnvironmentID, route.PublicHost)
	if err != nil {
		return 0, nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id FROM user_machine_terminal_sessions WHERE `+terminalLiveRowsPredicate+` ORDER BY created_at LIMIT 64`, userMachineID)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, terminalReconcileBudget)
	defer cancel()
	var (
		mu     sync.Mutex
		closed int
		wg     sync.WaitGroup
		jobs   = make(chan string)
	)
	for worker := 0; worker < terminalReconcileConcurrency && worker < len(ids); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				now := s.now().UTC()
				token, signErr := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: machine.EnvironmentID, MachineID: userMachineID, UserID: userID, CLIClientSessionID: newID("operation"), SessionID: id})
				if signErr != nil {
					continue
				}
				_, observeErr := s.controlRuntime.Terminal(ctx, ingress, token, "snapshot", id, newID("operation"))
				if observeErr == nil || !helperruntime.IsSessionUnknown(observeErr) {
					continue
				}
				if s.markTerminalSessionAbsent(ctx, id) == nil {
					mu.Lock()
					closed++
					mu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return closed, nil
}
