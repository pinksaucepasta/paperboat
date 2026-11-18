package breakglass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const (
	stagePending = "pending"
	stageRunning = "running"
	stageDone    = "done"
	stageSkipped = "skipped"
)

// StageState is one stage of a run: its state, when it ran and what it did (counts).
type StageState struct {
	Name       Stage          `json:"name"`
	State      string         `json:"state"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Detail     map[string]any `json:"detail"`
}

// Run is what the operator watches: the preview confirmed, the stages with their counts, and while a pickup wait is
// running the machines that hold the pending keys and those that do not yet.
type Run struct {
	ID           string                `json:"id"`
	State        string                `json:"state"`
	OperatorID   string                `json:"operator_id"`
	Reason       string                `json:"reason"`
	FloorVersion string                `json:"floor_version,omitempty"`
	Preview      Preview               `json:"preview"`
	Stages       []StageState          `json:"stages"`
	LastError    string                `json:"last_error,omitempty"`
	StartedAt    time.Time             `json:"started_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
	FinishedAt   *time.Time            `json:"finished_at,omitempty"`
	Pickups      map[Stage]PickupState `json:"pickups,omitempty"`
	keep         []byte
}

func jsonValue(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // a preview or stage list always marshals
	}
	return raw
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

const selectRun = `SELECT id, state, operator_id, reason, keep_session_hash, floor_version, preview, stages, last_error, started_at, updated_at, finished_at FROM paperboat.breakglass_runs`

func scanRun(row pgx.Row) (Run, error) {
	var run Run
	var preview, stages []byte
	if err := row.Scan(&run.ID, &run.State, &run.OperatorID, &run.Reason, &run.keep, &run.FloorVersion, &preview, &stages, &run.LastError, &run.StartedAt, &run.UpdatedAt, &run.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, err
	}
	if err := json.Unmarshal(preview, &run.Preview); err != nil {
		return Run{}, err
	}
	return run, json.Unmarshal(stages, &run.Stages)
}

// Get returns a run with its live pickup state; id "latest" is the newest run.
func (s *Service) Get(ctx context.Context, id string) (Run, error) {
	query, args := selectRun+` WHERE id = $1`, []any{id}
	if id == "latest" {
		query, args = selectRun+` ORDER BY started_at DESC LIMIT 1`, nil
	}
	run, err := scanRun(s.cfg.Store.Pool().QueryRow(ctx, query, args...))
	if err != nil {
		return Run{}, err
	}
	run.Pickups = map[Stage]PickupState{}
	for _, stage := range run.Stages {
		if since, ok := s.pickupSince(run, stage); ok && stage.State == stageRunning {
			if run.Pickups[stage.Name], err = s.pickup(ctx, since, stage.Name == StagePickup); err != nil {
				return Run{}, err
			}
		}
	}
	return run, nil
}

// Step advances the running run by one bounded unit of its current stage. It is called on a timer by one worker;
// a crash between steps loses nothing because each stage reads its progress from the database.
func (s *Service) Step(ctx context.Context) error {
	return s.cfg.Store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		run, err := scanRun(tx.QueryRow(ctx, selectRun+` WHERE state = 'running' FOR UPDATE SKIP LOCKED LIMIT 1`))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		index := -1
		for position, stage := range run.Stages {
			if stage.State == stagePending || stage.State == stageRunning {
				index = position
				break
			}
		}
		now := s.now()
		if index < 0 {
			if _, err = tx.Exec(ctx, `UPDATE paperboat.breakglass_runs SET state = 'done', finished_at = $2, updated_at = $2, last_error = '' WHERE id = $1`, run.ID, now); err != nil {
				return err
			}
			return s.writeAudit(ctx, tx, run.ID, run.OperatorID, "breakglass.finished", nil)
		}
		stage := &run.Stages[index]
		if stage.State == stagePending {
			stage.State, stage.StartedAt = stageRunning, &now
			if err = s.writeAudit(ctx, tx, run.ID, run.OperatorID, "breakglass.stage_started", map[string]any{"stage": stage.Name}); err != nil {
				return err
			}
		}
		done, execErr := s.execute(ctx, run, stage)
		run.LastError = ""
		if execErr != nil {
			run.LastError = fmt.Sprintf("stage %s: %v", stage.Name, execErr)
			slog.Error("break-glass step failed; it is retried", "run", run.ID, "stage", stage.Name, "error", execErr)
		}
		if done && execErr == nil {
			stage.State, stage.FinishedAt = stageDone, &now
			if err = s.writeAudit(ctx, tx, run.ID, run.OperatorID, "breakglass.stage_finished", map[string]any{"stage": stage.Name, "detail": stage.Detail}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE paperboat.breakglass_runs SET stages = $2, last_error = $3, updated_at = $4 WHERE id = $1`, run.ID, jsonValue(run.Stages), run.LastError, now)
		return err
	})
}

// Run steps on a timer until ctx ends; a failed step is logged by Step and retried on the next tick.
func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		if err := s.Step(ctx); err != nil && ctx.Err() == nil {
			slog.Error("break-glass worker", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

// add counts into a stage's detail; the detail round-trips through JSON, so numbers come back as float64.
func add(detail map[string]any, key string, n int) {
	current, _ := detail[key].(float64)
	if previous, ok := detail[key].(int); ok {
		current = float64(previous)
	}
	detail[key] = int(current) + n
}

func detailInt(detail map[string]any, key string) int {
	switch n := detail[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func detailTime(detail map[string]any, key string) (time.Time, bool) {
	raw, _ := detail[key].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	return at, err == nil
}
