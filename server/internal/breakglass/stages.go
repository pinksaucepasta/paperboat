package breakglass

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/mintwithdrawal"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// stepBatches bounds how many batches one step runs, so a step stays short and the worker comes back often.
const stepBatches = 20

// MachinePickup is one machine and whether it fetched its CA set since the keys were published.
type MachinePickup struct {
	MachineID string    `json:"machine_id"`
	Platform  string    `json:"platform"`
	FetchedAt time.Time `json:"fetched_at"`
	PickedUp  bool      `json:"picked_up"`
}

// PickupState is how many online machines hold the keys, and the machines that do not yet first.
type PickupState struct {
	Since    time.Time       `json:"since"`
	Expected int             `json:"expected"`
	PickedUp int             `json:"picked_up"`
	Machines []MachinePickup `json:"machines"`
}

// execute runs one bounded step of a stage and says whether the stage is finished.
func (s *Service) execute(ctx context.Context, run Run, stage *StageState) (bool, error) {
	switch stage.Name {
	case StagePublish:
		return s.publish(ctx, stage)
	case StagePickup:
		since, _ := s.pickupSince(run, *stage)
		return s.pickupDone(ctx, stage, since, true)
	case StageSwitch:
		return s.switchAccounts(ctx, stage)
	case StageRevoke:
		return s.revoke(ctx, run, stage)
	case StageReseal:
		return s.reseal(ctx, stage)
	case StageFloor:
		return s.raiseFloor(ctx, run, stage)
	case StageRetire:
		return s.retire(ctx, run, stage)
	}
	return false, fmt.Errorf("unknown stage %q", stage.Name)
}

// advanceAccounts moves up to a batch of accounts one CA step (publish adds the pending CA, retire removes the
// retiring one) and returns how many it moved. having selects the accounts still waiting for the step.
func (s *Service) advanceAccounts(ctx context.Context, stage *StageState, having, key string) (int, error) {
	ids, err := s.accounts(ctx, `SELECT account_id FROM paperboat.managed_ssh_user_cas GROUP BY account_id `+having+` ORDER BY account_id LIMIT $1`)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.cfg.SSH.AdvanceUserCA(ctx, id, s.now(), 0, 0); err != nil {
			return 0, fmt.Errorf("account %s: %w", id, err)
		}
		add(stage.Detail, key, 1)
	}
	return len(ids), s.wake(ctx, ids)
}

// wake tells every machine of the accounts to fetch its CA set now. Without it a host refreshes on its own timer
// (ten minutes), and every pickup wait would last that long.
func (s *Service) wake(ctx context.Context, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	return s.cfg.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM paperboat.user_machines WHERE user_id = ANY($1) AND revoked_at IS NULL AND deleted_at IS NULL`, accountIDs)
		if err != nil {
			return err
		}
		var machines []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			machines = append(machines, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(machines) == 0 {
			return err
		}
		for len(machines) > 0 { // Publish takes at most 1024 machines
			n := min(len(machines), 1000)
			if err := machineevents.Publish(ctx, tx, machines[:n], machineevents.TopicMachineAccess); err != nil {
				return err
			}
			machines = machines[n:]
		}
		return nil
	})
}

// publish adds the pending CA to every account that has only an active one. Accounts already mid-rotation (a
// retiring CA) are counted, not touched: that rotation finishes through retire.
func (s *Service) publish(ctx context.Context, stage *StageState) (bool, error) {
	moved, err := s.advanceAccounts(ctx, stage, `HAVING count(*) = 1 AND bool_and(state = 'active')`, "published")
	if err != nil || moved > 0 {
		return false, err
	}
	var mid int
	err = s.cfg.Store.Pool().QueryRow(ctx, `SELECT count(DISTINCT account_id) FROM paperboat.managed_ssh_user_cas WHERE state = 'retiring'`).Scan(&mid)
	stage.Detail["accounts_left_mid_rotation"] = mid
	return err == nil, err
}

func (s *Service) accounts(ctx context.Context, query string) ([]string, error) {
	rows, err := s.cfg.Store.Pool().Query(ctx, query, s.cfg.Batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// switchAccounts makes the pending CA sign and the old one retiring: no waiting (propagation 0), because the pickup
// stage already saw the machines hold it.
func (s *Service) switchAccounts(ctx context.Context, stage *StageState) (bool, error) {
	ids, err := s.accounts(ctx, `SELECT account_id FROM paperboat.managed_ssh_user_cas GROUP BY account_id HAVING bool_or(state = 'pending') AND NOT bool_or(state = 'retiring') ORDER BY account_id LIMIT $1`)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if _, err := s.cfg.SSH.AdvanceUserCA(ctx, id, s.now(), 0, 0); err != nil {
			return false, fmt.Errorf("account %s: %w", id, err)
		}
		add(stage.Detail, "switched", 1)
	}
	return len(ids) == 0, s.wake(ctx, ids)
}

// pickupSince is when the keys a stage waits for were published: the end of publish for the pickup stage, the end of
// the CA removal for retire.
func (s *Service) pickupSince(run Run, stage StageState) (time.Time, bool) {
	switch stage.Name {
	case StagePickup:
		if published := run.Stages[indexOf(StagePublish)]; published.FinishedAt != nil {
			return *published.FinishedAt, true
		}
	case StageRetire:
		return detailTime(stage.Detail, "removed_at")
	}
	return time.Time{}, false
}

// pickup counts the online machines (fetched their CA set within OnlineWindow before since) and those that fetched
// it since: a fetch after the change returns the changed set, so it is the proof a machine holds it. onlyPending
// limits the machines to those whose account has a pending CA.
func (s *Service) pickup(ctx context.Context, since time.Time, onlyPending bool) (PickupState, error) {
	state := PickupState{Since: since, Machines: []MachinePickup{}}
	filter := ""
	if onlyPending {
		filter = ` AND m.user_id IN (SELECT account_id FROM paperboat.managed_ssh_user_cas WHERE state = 'pending')`
	}
	rows, err := s.cfg.Store.Pool().Query(ctx, `SELECT f.machine_id, m.platform, f.fetched_at, f.fetched_at >= $2
		FROM paperboat.managed_ssh_ca_fetches f JOIN paperboat.user_machines m ON m.id = f.machine_id
		WHERE m.revoked_at IS NULL AND m.deleted_at IS NULL AND f.fetched_at >= $1`+filter+` ORDER BY f.fetched_at >= $2, f.machine_id`, since.Add(-s.cfg.OnlineWindow), since)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var machine MachinePickup
		if err := rows.Scan(&machine.MachineID, &machine.Platform, &machine.FetchedAt, &machine.PickedUp); err != nil {
			return state, err
		}
		state.Expected++
		if machine.PickedUp {
			state.PickedUp++
		}
		if len(state.Machines) < 200 {
			state.Machines = append(state.Machines, machine)
		}
	}
	return state, rows.Err()
}

// pickupDone finishes when the share of online machines that hold the keys reaches the threshold, or the timeout
// passes (offline machines pick the keys up when they reconnect; the detail says how many had not).
func (s *Service) pickupDone(ctx context.Context, stage *StageState, since time.Time, onlyPending bool) (bool, error) {
	state, err := s.pickup(ctx, since, onlyPending)
	if err != nil {
		return false, err
	}
	stage.Detail["expected"], stage.Detail["picked_up"] = state.Expected, state.PickedUp
	timedOut := stage.StartedAt != nil && s.now().Sub(*stage.StartedAt) >= s.cfg.PickupTimeout
	done := state.Expected == 0 || float64(state.PickedUp) >= s.cfg.PickupThreshold*float64(state.Expected) || timedOut
	if done {
		stage.Detail["timed_out"] = timedOut && state.PickedUp < state.Expected
	}
	return done, nil
}

// revoke ends every credential created before the stage began, except the acting operator's session.
func (s *Service) revoke(ctx context.Context, run Run, stage *StageState) (bool, error) {
	cutoff, ok := detailTime(stage.Detail, "cutoff")
	if !ok {
		cutoff = *stage.StartedAt
		stage.Detail["cutoff"] = cutoff.Format(time.RFC3339Nano)
	}
	total := 0
	revokeBatches := func(name, query string, args ...any) error {
		for range stepBatches {
			tag, err := s.cfg.Store.Pool().Exec(ctx, query, append([]any{s.cfg.Batch, cutoff}, args...)...)
			if err != nil {
				return fmt.Errorf("revoke %s: %w", name, err)
			}
			n := int(tag.RowsAffected())
			add(stage.Detail, name, n)
			total += n
			if n < s.cfg.Batch {
				break
			}
		}
		return nil
	}
	for _, statement := range revocations {
		query := `UPDATE paperboat.` + statement.table + ` SET ` + statement.set + ` WHERE ctid IN (SELECT ctid FROM paperboat.` + statement.table + ` WHERE ` + statement.live + ` AND ` + statement.created + ` <= $2 LIMIT $1)`
		if err := revokeBatches(statement.name, query); err != nil {
			return false, err
		}
	}
	operators := `UPDATE paperboat.operator_sessions SET revoked_at = now() WHERE ctid IN (SELECT ctid FROM paperboat.operator_sessions WHERE revoked_at IS NULL AND created_at <= $2 AND token_hash IS DISTINCT FROM $3 LIMIT $1)`
	if err := revokeBatches("operator_sessions", operators, run.keep); err != nil {
		return false, err
	}
	return total == 0, nil
}

// reseal moves every ciphertext to the newest key, a bounded batch per column at a time. A row that no key of the
// ring opens is counted and skipped: it was already unreadable, and it must not stop the run. The stage ends when
// every row not under the newest key is one of those.
func (s *Service) reseal(ctx context.Context, stage *StageState) (bool, error) {
	resealedNow, skipped := 0, 0
	for _, target := range sealedTargets {
		name := target.table + "." + target.column
		for range stepBatches {
			resealed, failed, err := s.resealBatch(ctx, target.table, target.column, detailInt(stage.Detail, "skip:"+name))
			if err != nil {
				return false, fmt.Errorf("reseal %s: %w", name, err)
			}
			add(stage.Detail, "resealed", resealed)
			add(stage.Detail, "skip:"+name, failed)
			resealedNow += resealed
			if resealed+failed < s.cfg.Batch {
				break
			}
		}
		skipped += detailInt(stage.Detail, "skip:"+name)
	}
	stage.Detail["undecryptable"] = skipped
	if resealedNow > 0 {
		return false, nil
	}
	remaining := 0
	for _, target := range sealedTargets {
		var n int
		if err := s.cfg.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM paperboat.`+target.table+` WHERE `+target.column+` IS NOT NULL AND substring(`+target.column+` from 1 for 12) <> $1`, secrets.Stamp(s.cfg.EncryptionKey)).Scan(&n); err != nil {
			return false, err
		}
		remaining += n
	}
	return remaining <= skipped, nil
}

func (s *Service) resealBatch(ctx context.Context, table, column string, skip int) (resealed, failed int, err error) {
	type row struct {
		ctid   string
		sealed []byte
	}
	err = s.cfg.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ctid::text, `+column+` FROM paperboat.`+table+` WHERE `+column+` IS NOT NULL AND substring(`+column+` from 1 for 12) <> $1
			ORDER BY ctid LIMIT $2 OFFSET $3 FOR UPDATE SKIP LOCKED`, secrets.Stamp(s.cfg.EncryptionKey), s.cfg.Batch, skip)
		if err != nil {
			return err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ctid, &r.sealed); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		resealed, failed = 0, 0
		for _, r := range batch {
			next, err := secrets.Reseal(s.cfg.EncryptionKey, r.sealed)
			if errors.Is(err, secrets.ErrDecrypt) {
				failed++
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE paperboat.`+table+` SET `+column+` = $1 WHERE ctid = $2::tid`, next, r.ctid); err != nil {
				return err
			}
			resealed++
		}
		return nil
	})
	return resealed, failed, err
}

// raiseFloor sets the client update floor, once. A floor already at or above the target is left alone.
func (s *Service) raiseFloor(ctx context.Context, run Run, stage *StageState) (bool, error) {
	policy, err := s.cfg.Floor.Get(ctx)
	if err != nil {
		return false, err
	}
	target, _ := clientupdate.ParseVersion(run.FloorVersion)
	if current, ok := clientupdate.ParseVersion(policy.MinimumVersion); ok && clientupdate.Compare(current, target) >= 0 {
		stage.Detail["floor"] = policy.MinimumVersion
		return true, nil
	}
	_, err = s.cfg.Floor.Put(ctx, run.OperatorID, "breakglass-"+run.ID, "breakglass:"+run.ID, clientupdate.Input{ExpectedRevision: policy.Revision, MinimumVersion: run.FloorVersion, Reason: "break-glass " + run.ID})
	if errors.Is(err, clientupdate.ErrConflict) {
		return false, nil // the policy changed under us: read it again next step
	}
	if err == nil {
		stage.Detail["floor"] = run.FloorVersion
	}
	return err == nil, err
}

// retire refuses the old keys: the retiring CAs are removed at once (not when their certificates expire: they may
// be forged), the old mint keys are withdrawn, and the stage ends when the machines fetched the CA set without them.
func (s *Service) retire(ctx context.Context, run Run, stage *StageState) (bool, error) {
	if _, done := stage.Detail["mint_withdrawn"]; !done {
		active, published := s.cfg.Mint.KeyIDs()
		withdrawn := []string{}
		for _, id := range published {
			if id == active {
				continue
			}
			if err := mintwithdrawal.Withdraw(ctx, s.cfg.Store.Pool(), id, active, run.OperatorID, "break-glass "+run.ID); err != nil {
				return false, err
			}
			withdrawn = append(withdrawn, id)
		}
		stage.Detail["mint_withdrawn"] = withdrawn
	}
	if _, removed := detailTime(stage.Detail, "removed_at"); !removed {
		moved, err := s.advanceAccounts(ctx, stage, `HAVING bool_or(state = 'retiring')`, "cas_removed")
		if err != nil || moved > 0 {
			return false, err
		}
		stage.Detail["removed_at"] = s.now().Format(time.RFC3339Nano)
		return false, nil
	}
	since, _ := detailTime(stage.Detail, "removed_at")
	return s.pickupDone(ctx, stage, since, false)
}
