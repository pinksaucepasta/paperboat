package usermachines

import (
	"context"
	"errors"
)

// MachineSSH is a listed machine's managed SSH target, present only when the caller may use
// managed SSH on it and the target belongs to its current installation. It carries what every
// daemon otherwise fetched per machine from /ssh-target and /ssh-host-keys on each inventory
// refresh (N machines x N lookups across an account). HostKeyGeneration is 0 until the machine
// has an active host key set for its current installation.
type MachineSSH struct {
	OSUser            string `json:"os_user"`
	Port              int32  `json:"port"`
	TargetGeneration  int64  `json:"target_generation"`
	HostKeyGeneration int64  `json:"host_key_generation"`
}

// describeManagedSSH fills MachineSSH with one lock-free query. The authority predicate is the
// one GET /ssh-target and /ssh-host-keys apply (ResolveMachineSSHUseAuthorityForUpdate); the
// caller's own live personal machines are answered first, as the access functions' first branch
// does, so they cost no function calls.
func (s *Service) describeManagedSSH(ctx context.Context, account string, machines ...*UserMachine) error {
	if len(machines) == 0 {
		return nil
	}
	ids := make([]string, 0, len(machines))
	byID := make(map[string]*UserMachine, len(machines))
	for _, machine := range machines {
		ids = append(ids, machine.ID)
		byID[machine.ID] = machine
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id,t.os_user,t.target_port,t.machine_generation,coalesce(h.machine_generation,0)
 FROM user_machines m
 JOIN machine_ssh_targets t ON t.user_machine_id=m.id AND t.machine_generation=m.installation_generation
 LEFT JOIN machine_ssh_host_key_sets h ON h.user_machine_id=m.id AND h.state='active' AND h.machine_generation=m.installation_generation
 WHERE m.id=ANY($2::text[]) AND m.state NOT IN ('revoked','disconnected','deleted')
 AND ((m.owner_team_id IS NULL AND m.user_id=$1 AND m.revoked_at IS NULL AND m.deleted_at IS NULL)
  OR machine_capability_allowed($1,m.id,'managed_ssh') OR machine_management_allowed($1,m.id))`, account, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var ssh MachineSSH
		if err := rows.Scan(&id, &ssh.OSUser, &ssh.Port, &ssh.TargetGeneration, &ssh.HostKeyGeneration); err != nil {
			return errors.Join(err, rows.Close())
		}
		if machine := byID[id]; machine != nil {
			machine.SSH = &ssh
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}
