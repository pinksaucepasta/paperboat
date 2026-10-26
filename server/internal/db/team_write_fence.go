package db

import (
	"context"
	"fmt"
	"hash/fnv"
)

// LockTeamWrites serializes team-touching writes (membership, grants, policy, access sessions). It must be
// the first statement of the transaction: LOCK TABLE takes no snapshot, so a SERIALIZABLE transaction's
// snapshot is taken after the lock is granted and sees every write of the transaction it waited for.
// (An advisory lock taken by a SELECT took the snapshot before waiting, and waiters failed with 40001.)
func LockTeamWrites(ctx context.Context, tx *Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE paperboat.team_write_fence IN EXCLUSIVE MODE`)
	return err
}

// TeamWriteFenceBuckets is how many lock buckets team writes spread over (migration 192).
const TeamWriteFenceBuckets = 16

// TeamWriteFenceBucket is the bucket a team's writes lock.
func TeamWriteFenceBucket(team string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(team))
	return int(h.Sum32() % TeamWriteFenceBuckets)
}

// LockTeamWritesFor serializes writes that touch only this team: they wait for the global lock and for
// writes of teams in the same bucket, not for other teams. It must be the first statement of the
// transaction, like LockTeamWrites. A transaction that later needs the global lock may deadlock with
// another one doing the same; PostgreSQL ends one (40P01) and InTx retries it.
func LockTeamWritesFor(ctx context.Context, tx *Tx, team string) error {
	if _, err := tx.Exec(ctx, `LOCK TABLE paperboat.team_write_fence IN ROW SHARE MODE`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE paperboat.team_write_fence_%02d IN EXCLUSIVE MODE`, TeamWriteFenceBucket(team)))
	return err
}
