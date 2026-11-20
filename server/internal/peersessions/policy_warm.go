package peersessions

import (
	"context"
	"sync"
	"time"
)

// WarmPolicies fills the policy cache for up to limit online machines before the server reports ready. After a
// restart the cache is empty and every machine reconnects at once, each needing a document built from PostgreSQL
// (~300 ms on a large team); the warm-up builds them with bounded concurrency first, for the key an idle daemon asks
// with (until-revoked authority, no admitted scopes). A document built here is the one the machine's first refresh
// finds, exactly as if an earlier refresh had built it. It waits for the LISTEN session (a document is stored only
// while one is attached) and returns the number cached; ctx bounds it all, and anything not built is built on demand.
func (s *NetworkService) WarmPolicies(ctx context.Context, limit, workers int) (int, error) {
	if limit < 1 || workers < 1 {
		return 0, ErrInvalid
	}
	for {
		if _, listening := s.cache.begin(); listening {
			break
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT user_id,id FROM paperboat.user_machines
		WHERE state='online' AND online AND revoked_at IS NULL AND deleted_at IS NULL ORDER BY user_id,id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var machines [][2]string
	for rows.Next() {
		var m [2]string
		if err := rows.Scan(&m[0], &m[1]); err != nil {
			_ = rows.Close()
			return 0, err
		}
		machines = append(machines, m)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	jobs := make(chan [2]string)
	var group sync.WaitGroup
	var mu sync.Mutex
	warmed := 0
	for range workers {
		group.Go(func() {
			for m := range jobs {
				if s.warmPolicy(ctx, m[0], m[1]) {
					mu.Lock()
					warmed++
					mu.Unlock()
				}
			}
		})
	}
	for _, m := range machines {
		select {
		case jobs <- m:
		case <-ctx.Done():
		}
	}
	close(jobs)
	group.Wait()
	return warmed, ctx.Err()
}

// warmPolicy builds and stores one machine's document; a machine that cannot be built (revoked since the listing,
// no identity yet) is skipped, its first request will answer for itself.
func (s *NetworkService) warmPolicy(ctx context.Context, user, endpoint string) bool {
	in := NetworkConfigRequest{UserID: user, EndpointID: endpoint, Authority: AuthorityUntilRevoked}
	live, err := admittedScopeIDs(in.AdmittedScopes)
	if err != nil {
		return false
	}
	start, cacheable := s.cache.begin()
	doc, err := s.buildPolicy(ctx, in, live, s.now().UTC())
	if err != nil || !cacheable {
		return false
	}
	doc.liveKey = policyDocKey(in, live)
	s.cache.put(endpoint, doc, start)
	return true
}
