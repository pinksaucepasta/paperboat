package teams

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const (
	// AdmissionAccepted: the machine is shared into the team right away.
	AdmissionAccepted = "accepted"
	// AdmissionPending: the machine waits for an owner or admin.
	AdmissionPending = "pending"
)

// MachineAdmission is a machine that asked to join a team and has not been accepted: pending, or
// declined (kept so its owner sees the answer).
type MachineAdmission struct {
	MachineID   string     `json:"machine_id"`
	Alias       string     `json:"alias"`
	RequestedBy string     `json:"requested_by"`
	State       string     `json:"state"`
	RequestedAt time.Time  `json:"requested_at"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// admissionFor is what happens to a machine the actor adds: owners and admins add directly, other
// members wait when the team asks for approval.
func admissionFor(t Team, actor Member) string {
	if t.ApproveNewMachines && !Allowed(actor.Role, PermissionSettingsManage) {
		return AdmissionPending
	}
	return AdmissionAccepted
}

// AdmissionFor tells what adding a machine to the team would do for this account.
func (s *Service) AdmissionFor(ctx context.Context, account, team string) (string, error) {
	var out string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := Lock(ctx, tx); err != nil {
			return err
		}
		t, err := ReadTx(ctx, tx, team)
		if err != nil {
			return err
		}
		actor, ok := member(t, account)
		if t.Deleted || !ok {
			return ErrForbidden
		}
		out = admissionFor(t, actor)
		return nil
	})
	return out, err
}

func requestMachineTx(ctx context.Context, tx *db.Tx, team, machine, account string) error {
	_, err := tx.Exec(ctx, `INSERT INTO team_machine_requests(team_id,machine_id,requested_by,state) VALUES($1,$2,$3,'pending') ON CONFLICT(team_id,machine_id) DO UPDATE SET requested_by=EXCLUDED.requested_by,state='pending',requested_at=now(),decided_by=NULL,decided_at=NULL`, team, machine, account)
	return err
}

func readMachineRequestsTx(ctx context.Context, tx *db.Tx, team string, out *Team) error {
	rows, err := tx.Query(ctx, `SELECT r.machine_id,m.alias,r.requested_by,r.state,r.requested_at,coalesce(r.decided_by,''),r.decided_at FROM team_machine_requests r JOIN live_user_machines m ON m.id=r.machine_id WHERE r.team_id=$1 ORDER BY r.requested_at,r.machine_id`, team)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a MachineAdmission
		if err = rows.Scan(&a.MachineID, &a.Alias, &a.RequestedBy, &a.State, &a.RequestedAt, &a.DecidedBy, &a.DecidedAt); err != nil {
			return err
		}
		out.MachineRequests = append(out.MachineRequests, a)
	}
	return rows.Err()
}

// ViewFor hides other members' machine requests from members who are not owners or admins.
func ViewFor(t Team, account string) Team {
	if actor, ok := currentMember(t, account); ok && Allowed(actor.Role, PermissionSettingsManage) {
		return t
	}
	own := []MachineAdmission{}
	for _, request := range t.MachineRequests {
		if request.RequestedBy == account {
			own = append(own, request)
		}
	}
	t.MachineRequests = own
	return t
}

// requestRowTx locks the pending or declined request of a machine.
func requestRowTx(ctx context.Context, tx *db.Tx, team, machine string) (requestedBy string, err error) {
	err = tx.QueryRow(ctx, `SELECT requested_by FROM team_machine_requests WHERE team_id=$1 AND machine_id=$2 FOR UPDATE`, team, machine).Scan(&requestedBy)
	if err == pgx.ErrNoRows {
		err = ErrNotFound
	}
	return requestedBy, err
}
