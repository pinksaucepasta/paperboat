package db

import "context"

// LockTeamWrites serializes team-touching writes (membership, grants, policy, access sessions). It must be
// the first statement of the transaction: LOCK TABLE takes no snapshot, so a SERIALIZABLE transaction's
// snapshot is taken after the lock is granted and sees every write of the transaction it waited for.
// (An advisory lock taken by a SELECT took the snapshot before waiting, and waiters failed with 40001.)
func LockTeamWrites(ctx context.Context, tx *Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE paperboat.team_write_fence IN EXCLUSIVE MODE`)
	return err
}
