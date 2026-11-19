package peersessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"sync"
)

// meshCache keeps each account's mesh (endpoint -> entry) as of a mesh_seq in memory. One change makes every
// endpoint of the account renew: each renewal used to read the account's whole mesh log (DISTINCT ON over an hour
// of superseded entries) and decode every entry, N renewals x N entries per change. Now the first renewal at a new
// seq reads only the log entries after the seq held here and the others reuse its result: a change costs its
// changed entries. Like headscale's batcher, which keeps the node set in memory and applies change sets to it
// (hscontrol/mapper/batcher.go, change notifications in hscontrol/types/change), and tailscale's control, which
// sends PeersChanged/PeersRemoved from a per-node peer set rather than recomputing it (tailcfg.MapResponse).
//
// The log is append-only below the retention worker, which only deletes an entry a later entry of its endpoint
// replaced (still read here) or a remove kept for days (config.Retention.EndedSessions); so applying the entries
// in (held, seq] to the held mesh gives exactly latestMeshEntries at seq. A reader at an older seq than the one
// held (a renewal racing a newer change) reads the log as before.
type meshCache struct {
	mu       sync.Mutex
	accounts map[string]meshSnapshot
	// rows counts the log entries read (tests assert a change reads its changed entries only).
	rows int
}

type meshSnapshot struct {
	seq     int64
	entries map[string]networkBinding // never changed in place: readers share it
}

// maxMeshAccounts bounds memory: an entry is ~1 kB, so 10k accounts of 100 endpoints is about a GB at worst.
const maxMeshAccounts = 10_000

// at returns the account's mesh at seq, read in tx (a snapshot that sees every log entry up to seq). Callers must
// not modify the returned map.
func (c *meshCache) at(ctx context.Context, tx *sql.Tx, account string, seq int64) (map[string]networkBinding, error) {
	c.mu.Lock()
	held, ok := c.accounts[account]
	c.mu.Unlock()
	if ok && held.seq == seq {
		return held.entries, nil
	}
	if !ok || held.seq > seq {
		entries, err := latestMeshEntries(ctx, tx, account, seq)
		if err != nil {
			return nil, err
		}
		c.store(account, meshSnapshot{seq, entries}, len(entries))
		return entries, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT endpoint_id,op,entry FROM paperboat.peer_mesh_log
 WHERE account_id=$1 AND mesh_seq>$2 AND mesh_seq<=$3 ORDER BY mesh_seq`, account, held.seq, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, read := maps.Clone(held.entries), 0
	for rows.Next() {
		read++
		if err := scanMeshEntry(rows, entries); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.store(account, meshSnapshot{seq, entries}, read)
	return entries, nil
}

// store keeps snap unless a newer seq of the account is held already.
func (c *meshCache) store(account string, snap meshSnapshot, read int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows += read
	if c.accounts == nil {
		c.accounts = map[string]meshSnapshot{}
	}
	if held, ok := c.accounts[account]; ok && held.seq >= snap.seq {
		return
	}
	if _, ok := c.accounts[account]; !ok && len(c.accounts) >= maxMeshAccounts {
		for id := range c.accounts { // map order: an arbitrary victim, which then reads its log once
			delete(c.accounts, id)
			break
		}
	}
	c.accounts[account] = snap
}

// scanMeshEntry applies one log row (endpoint_id, op, entry) to entries.
func scanMeshEntry(rows *sql.Rows, entries map[string]networkBinding) error {
	var endpoint, op string
	var raw []byte
	if err := rows.Scan(&endpoint, &op, &raw); err != nil {
		return err
	}
	if op == "remove" {
		delete(entries, endpoint)
		return nil
	}
	var b networkBinding
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	entries[endpoint] = b
	return nil
}
