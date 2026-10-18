package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// MaxInboxItems bounds what a machine reports and what the console lists.
const MaxInboxItems = 200

var (
	ErrInboxObservationInvalid = errors.New("inbox observation is invalid")
	ErrInboxObservationStale   = errors.New("inbox observation is older than the stored one")
)

// InboxItem is one file in a machine's Paperboat Inbox, as the machine saw it.
type InboxItem struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"received_at"`
}

// InboxObservation is the machine's listing of its Inbox: newest first, at most MaxInboxItems of Total.
type InboxObservation struct {
	ObservedAt time.Time   `json:"observed_at"`
	Total      int         `json:"total"`
	Items      []InboxItem `json:"items"`
}

// InboxListing is what the console reads: the last reported listing and when it was observed.
type InboxListing struct {
	ObservedAt *time.Time  `json:"observed_at"`
	Total      int         `json:"total"`
	Items      []InboxItem `json:"items"`
}

func (o InboxObservation) Validate(now time.Time) error {
	if o.ObservedAt.IsZero() || o.ObservedAt.After(now.Add(5*time.Minute)) || o.Total < 0 || len(o.Items) > MaxInboxItems || o.Total < len(o.Items) {
		return ErrInboxObservationInvalid
	}
	seen := make(map[string]bool, len(o.Items))
	for _, item := range o.Items {
		name := strings.TrimSpace(item.Name)
		if item.ID == "" || len(item.ID) > 64 || seen[item.ID] || name == "" || len(item.Name) > 255 || !utf8.ValidString(item.Name) || strings.ContainsAny(item.Name, "/\\\x00\r\n") || item.Size < 0 || item.ReceivedAt.IsZero() {
			return ErrInboxObservationInvalid
		}
		seen[item.ID] = true
	}
	return nil
}

// RecordInboxObservation stores a machine's Inbox listing. The machine is identified by its runtime
// environment, as for update observations; an older listing never replaces a newer one.
func (s *Service) RecordInboxObservation(ctx context.Context, environmentID, userMachineID string, observation InboxObservation) error {
	environmentID, userMachineID = strings.TrimSpace(environmentID), strings.TrimSpace(userMachineID)
	if environmentID == "" || userMachineID == "" || observation.Validate(s.now().UTC()) != nil {
		return ErrInboxObservationInvalid
	}
	items := observation.Items
	if items == nil {
		items = []InboxItem{}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return ErrInboxObservationInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		machine, err := tx.Queries().GetUserMachineForEnvironmentBandwidthUpdate(ctx, environmentID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && machine.ID != userMachineID {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO machine_inbox_observations(user_machine_id,environment_id,items,total,observed_at)
 VALUES($1,$2,$3::jsonb,$4,$5)
 ON CONFLICT (user_machine_id) DO UPDATE SET environment_id=EXCLUDED.environment_id,items=EXCLUDED.items,total=EXCLUDED.total,observed_at=EXCLUDED.observed_at,updated_at=now()
 WHERE machine_inbox_observations.observed_at <= EXCLUDED.observed_at`, userMachineID, environmentID, string(encoded), observation.Total, observation.ObservedAt.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInboxObservationStale
		}
		return nil
	})
}

// ListInbox returns the last Inbox listing a machine reported. Only the machine's owner, or an owner
// or admin of the team that owns it, may read it: file names are private. A machine that has not
// reported yet has an empty listing with no observation time.
func (s *Service) ListInbox(ctx context.Context, userID, userMachineID string) (InboxListing, error) {
	result := InboxListing{Items: []InboxItem{}}
	_, teamID := workspaceTeamArgument(ctx)
	var raw []byte
	var observed sql.NullTime
	var total sql.NullInt64
	err := s.db.SQL().QueryRowContext(ctx, `SELECT o.items,o.total,o.observed_at
 FROM user_machines m LEFT JOIN machine_inbox_observations o ON o.user_machine_id=m.id
 WHERE m.id=$1 AND m.deleted_at IS NULL AND machine_management_allowed($2,m.id)
  AND ($3::text IS NULL OR m.owner_team_id=$3)`, userMachineID, userID, teamID).Scan(&raw, &total, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if observed.Valid {
		at := observed.Time.UTC()
		result.ObservedAt = &at
		result.Total = int(total.Int64)
		if err := json.Unmarshal(raw, &result.Items); err != nil {
			return InboxListing{Items: []InboxItem{}}, err
		}
	}
	return result, nil
}
