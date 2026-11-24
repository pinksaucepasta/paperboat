package peersessions

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// accountBasis is the part of a peer policy document that does not depend on the endpoint asking. One account change
// makes every endpoint of the account rebuild its document; each used to read these rows again. It lives in the
// policy cache under its own keys, so the notifications that drop documents drop it too, and ends at its earliest
// time-based input.
type accountBasis struct {
	started time.Time
	mesh    meshState
	members []basisMember
	// entitlement is the next instant a team's paid period starts or ends (zero: none).
	entitlement time.Time
}

type basisMember struct{ endpoint, role string }

// accountBasis returns the account's basis, built once per change.
//
// The change signal is read on every call: a stored basis is reused only while the signal equals the one its mesh was
// built at and the mesh is younger than meshMaxAge, exactly the test refreshMesh makes under its lock. The signal also
// covers writes no policy notification names (a relay revocation generation), so the notifications alone do not do.
// A basis whose build began after this call read a state at least as new as this call's own build would.
func (s *NetworkService) accountBasis(ctx context.Context, account string, now time.Time) (*accountBasis, error) {
	started := time.Now()
	signal, err := meshSignal(ctx, s.store.SQL(), account)
	if err != nil {
		return nil, err
	}
	doc, err := s.sharedDoc(ctx, "basis:"+account, now, func(d *policyDoc) bool {
		return d.basis.mesh.signal == signal || d.basis.started.After(started)
	}, func() (*policyDoc, error) { return s.buildBasis(ctx, account, now) })
	if err != nil {
		return nil, err
	}
	return doc.basis, nil
}

// sharedDoc returns the account-wide entry under key, built by one caller at a time while the cache holds it.
func (s *NetworkService) sharedDoc(ctx context.Context, key string, now time.Time, current func(*policyDoc) bool, build func() (*policyDoc, error)) (*policyDoc, error) {
	if _, listening := s.cache.begin(); !listening {
		return build() // nothing is stored, so nothing is shared
	}
	if d := s.cache.shared(key, now); d != nil && current(d) {
		return d, nil
	}
	unlock, err := s.basisBuilds.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if d := s.cache.shared(key, now); d != nil && current(d) {
		return d, nil
	}
	// put refuses an entry built across a notification for one of its keys: the next caller builds its own.
	start, _ := s.cache.begin()
	d, err := build()
	if err != nil {
		return nil, err
	}
	s.cache.put(key, d, start)
	return d, nil
}

// teamHosts is teamHostPeers for the account, shared by its CLI endpoints. The hosts are other accounts' machines, so
// no change signal covers them: the notifications of the hosts, their accounts and this account do.
func (s *NetworkService) teamHosts(ctx context.Context, account string, entitlement, now time.Time) ([]offlinePeer, error) {
	doc, err := s.sharedDoc(ctx, "hosts:"+account, now, func(*policyDoc) bool { return true }, func() (*policyDoc, error) {
		tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		hosts, err := teamHostPeers(ctx, tx, account, now)
		if err != nil {
			return nil, err
		}
		keys, ends := []string{"account:" + account}, []time.Time{entitlement}
		// Every machine a team shares with the account is a key, in the answer or not: what makes a left-out host
		// valid (its machine, environment, identity, certificate or owner) names the machine or its owner's account.
		shared, err := tx.QueryContext(ctx, `SELECT b.resource_id,m.user_id FROM paperboat.team_members member
 JOIN paperboat.team_resource_bindings b ON b.team_id=member.team_id AND b.resource_kind='machine'
 JOIN paperboat.user_machines m ON m.id=b.resource_id WHERE member.account_id=$1 LIMIT $2`, account, maximumSharedHostKeys+1)
		if err != nil {
			return nil, err
		}
		defer shared.Close()
		for shared.Next() {
			var machine, owner string
			if err = shared.Scan(&machine, &owner); err != nil {
				return nil, err
			}
			keys = append(keys, machine, "account:"+owner)
		}
		if err = shared.Err(); err != nil {
			return nil, err
		}
		// A team's SSO session (paperboat.team_member_sso_ok) ends by time, with no write.
		var sso sql.NullTime
		if err = tx.QueryRowContext(ctx, `SELECT min(d) FROM (SELECT m.sso_verified_at+make_interval(secs => t.sso_session_seconds) AS d
 FROM paperboat.team_members m JOIN paperboat.teams t ON t.team_id=m.team_id AND t.sso_required WHERE m.account_id=$1 AND m.active) x WHERE d>$2`, account, now).Scan(&sso); err != nil {
			return nil, err
		}
		ends = append(ends, sso.Time)
		if len(keys) > 2*maximumSharedHostKeys+1 {
			ends = append(ends, now) // more machines than keys are kept for: built per request, never stored
		}
		for _, h := range hosts {
			keys = append(keys, h.binding.EndpointID, h.binding.MachineID, "account:"+h.binding.AccountID)
			ends = append(ends, h.certExpiry)
		}
		return sharedEntry(&policyDoc{hosts: hosts, validUntil: now.Add(meshMaxAge)}, keys, ends), tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return doc.hosts, nil
}

// maximumSharedHostKeys bounds the machines one stored host list depends on.
const maximumSharedHostKeys = 4096

// sharedEntry sets d's keys and ends it at the earliest of ends (zero: no end).
func sharedEntry(d *policyDoc, keys []string, ends []time.Time) *policyDoc {
	for _, end := range ends {
		if !end.IsZero() && end.Before(d.validUntil) {
			d.validUntil = end
		}
	}
	slices.Sort(keys)
	d.keys = slices.Compact(keys)
	return d
}

func (s *NetworkService) buildBasis(ctx context.Context, account string, now time.Time) (*policyDoc, error) {
	b := &accountBasis{started: time.Now()}
	var err error
	// The shared mesh is brought up to date (once per change for the whole account) before the snapshot opens, so
	// the snapshot sees it; this writes.
	if b.mesh, err = s.refreshMesh(ctx, account); err != nil {
		return nil, err
	}
	tx, err := s.store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Machines first: the bound holds every machine or every endpoint beyond the product maximum, so the filter per
	// endpoint (personalNetworkPeers) refuses exactly the accounts a query per endpoint refused.
	rows, err := tx.QueryContext(ctx, `SELECT n.endpoint_id,n.role FROM paperboat.peer_network_identities n
 WHERE n.user_id=$1 AND n.revoked_at IS NULL AND
 ((n.role='machine' AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=n.machine_id AND m.user_id=n.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL))
 OR (n.role='cli' AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=n.endpoint_id AND c.user_id=n.user_id AND c.state='active' AND c.revoked_at IS NULL)))
 ORDER BY n.role='machine' DESC,n.endpoint_id LIMIT $2`, account, maximumAccountEndpoints+2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{"account:" + account}
	for rows.Next() {
		var m basisMember
		if err = rows.Scan(&m.endpoint, &m.role); err != nil {
			return nil, err
		}
		b.members = append(b.members, m)
		keys = append(keys, m.endpoint) // a machine's endpoint id is its machine id
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// A team's paid entitlement (paperboat.team_paid_entitled) starts and ends by time, with no write: a document
	// holds only until the next such instant of a team the account belongs to or a machine of it is shared with.
	var entitlement sql.NullTime
	if err = tx.QueryRowContext(ctx, `WITH teams AS (
 SELECT team_id FROM paperboat.team_members WHERE account_id=$1 AND active
 UNION SELECT r.team_id FROM paperboat.user_machines m JOIN paperboat.team_resource_bindings r ON r.resource_kind='machine' AND r.resource_id=m.id AND r.active WHERE m.user_id=$1
 UNION SELECT owner_team_id FROM paperboat.user_machines WHERE user_id=$1 AND owner_team_id IS NOT NULL)
SELECT min(t) FROM (
 SELECT w.period_start AS t FROM paperboat.billing_workspaces w JOIN teams USING (team_id) WHERE w.user_id IS NULL
 UNION ALL SELECT paperboat.billing_workspace_paid_until(w) FROM paperboat.billing_workspaces w JOIN teams USING (team_id) WHERE w.user_id IS NULL) x
WHERE t>$2`, account, now).Scan(&entitlement); err != nil {
		return nil, err
	}
	b.entitlement = entitlement.Time
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return sharedEntry(&policyDoc{basis: b, validUntil: b.mesh.builtAt.Add(meshMaxAge)}, keys, []time.Time{b.mesh.validUntil, b.entitlement}), nil
}
