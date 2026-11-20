package accountingauthority

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// RecordMany admits bindings, one answer per binding in order (nil: admitted), exactly as Record does for each of them:
// the node's lock and one transaction cover a whole group of one node's bindings instead of one each. An edge's ingress
// pull re-admits every route it serves: at 1,000 tunnels that was 1,000 transactions of 13 statements each, ~10 s, longer
// than the 10 s life of the decisions it produced, so the edge rejected them and served only the routes it had before
// (F21-168). The workspace's period row, the node's customer-owned check and the quota are read once per workspace,
// the node's capacity once per group, and the rows are written by one statement.
func RecordMany(ctx context.Context, store *db.DB, bindings []Binding, policy QuotaChecker) []error {
	errs := make([]error, len(bindings))
	groups := map[string][]int{}
	for i, b := range bindings {
		if policy == nil || b.ID == "" || b.Node == "" || b.Environment == "" || b.Route == "" || b.User == "" || len(b.User) > 256 || b.Revision < 1 || b.ExpiresAt.Before(b.IssuedAt) {
			errs[i] = ErrUnavailable
			continue
		}
		groups[b.Node] = append(groups[b.Node], i)
	}
	for node, indexes := range groups {
		recordNode(ctx, store, node, bindings, indexes, policy, errs)
	}
	return errs
}

// recordNode admits one node's bindings. The node lock is taken before the transaction starts: taken inside it, a waiter
// (an edge's old and new process pulling at once during a switch) decided on a snapshot from before the wait and failed
// with 40001 until its retries ran out (503 on the edge's desired-state pull, 2026-11-12 17:17Z). Within this process the
// node's callers queue on a mutex, so only one at a time polls the session lock (its backoff let a busy node's waiters
// lose for over a second); the session lock orders processes.
func recordNode(ctx context.Context, store *db.DB, node string, bindings []Binding, indexes []int, policy QuotaChecker, errs []error) {
	nodeLock := nodeLocks[nodeStripe(node)]
	select {
	case nodeLock <- struct{}{}:
	case <-ctx.Done():
		fail(errs, indexes, ctx.Err())
		return
	}
	defer func() { <-nodeLock }()
	err := store.InReadCommittedTxUnderSessionLock(ctx, node, 369197, func(ctx context.Context, tx *db.Tx) error {
		for _, i := range indexes {
			errs[i] = nil // the transaction may be replayed
		}
		return recordNodeTx(ctx, tx, node, bindings, indexes, policy, errs)
	})
	if err != nil {
		fail(errs, indexes, err)
	}
}

func fail(errs []error, indexes []int, err error) {
	for _, i := range indexes {
		errs[i] = err
	}
}

type workspaceAdmission struct {
	id        string
	exhausted error
}

func recordNodeTx(ctx context.Context, tx *db.Tx, node string, bindings []Binding, indexes []int, policy QuotaChecker, errs []error) error {
	issued := bindings[indexes[0]].IssuedAt
	if _, err := tx.Exec(ctx, `DELETE FROM usage_accounting_authorities WHERE edge_node_id=$1 AND retain_until<=$2`, node, issued); err != nil {
		return err
	}
	// Customer-owned transport is observed but never consumes Paperboat's bandwidth allowance. Classification comes from
	// the registry, not the caller; globally operated installations remain billable hosted nodes.
	var customerOwned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM selfhost_installations WHERE node_id=$1 AND scope_kind IN ('account','team'))`, node).Scan(&customerOwned); err != nil {
		return err
	}
	workspaces := map[string]*workspaceAdmission{}
	var candidates []int
	for _, i := range indexes {
		b := bindings[i]
		key := b.Team + "\x00" + b.User
		w := workspaces[key]
		if w == nil {
			var err error
			if w, err = admitWorkspaceTx(ctx, tx, b, customerOwned, policy); err != nil {
				return err
			}
			workspaces[key] = w
		}
		// A throttled binding is admitted like any other (its bytes are still recorded); the answer tells the caller.
		errs[i] = w.exhausted
		candidates = append(candidates, i)
	}
	if len(candidates) == 0 {
		return nil
	}
	return retainTx(ctx, tx, node, bindings, candidates, workspaces, errs)
}

// admitWorkspaceTx creates the payer's workspace row if needed, locks it, and judges its bandwidth allowance.
func admitWorkspaceTx(ctx context.Context, tx *db.Tx, b Binding, customerOwned bool, policy QuotaChecker) (*workspaceAdmission, error) {
	var err error
	if b.Team != "" {
		_, err = tx.Exec(ctx, `INSERT INTO billing_workspaces(id,team_id) SELECT $1::text,$2::text WHERE NOT EXISTS(SELECT 1 FROM billing_workspaces WHERE id=$1) ON CONFLICT DO NOTHING`, "team:"+b.Team, b.Team)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO billing_workspaces(id,user_id) SELECT $1::text,$2::text WHERE NOT EXISTS(SELECT 1 FROM billing_workspaces WHERE id=$1) ON CONFLICT DO NOTHING`, "personal:"+b.User, b.User)
	}
	if err != nil {
		return nil, err
	}
	// Payer identity is the unique owning account/team, not the suggested ID used when creating a workspace for the first time.
	w := &workspaceAdmission{}
	if err = tx.QueryRow(ctx, `SELECT id FROM billing_workspaces WHERE (team_id=$1 AND $1<>'' AND user_id IS NULL) OR (user_id=$2 AND $1='' AND team_id IS NULL) FOR UPDATE`, b.Team, b.User).Scan(&w.id); err != nil {
		return nil, err
	}
	if customerOwned {
		return w, nil
	}
	exhausted, err := policy.CheckWorkspaceBandwidthTx(ctx, tx, w.id, b.IssuedAt)
	if err != nil {
		return nil, err
	}
	if exhausted {
		w.exhausted = throttledTx(ctx, tx, w.id, b.IssuedAt)
		if _, ok := w.exhausted.(*ThrottledError); !ok {
			return nil, w.exhausted
		}
	}
	return w, nil
}

func bindingKey(authority, environment, route string, revision int64) string {
	return authority + "\x00" + environment + "\x00" + route + "\x00" + strconv.FormatInt(revision, 10)
}

// retainTx records the admitted bindings with one statement. A node holds any number of them: capacity is watched by resource
// alerts, not by a count that would refuse the 4,097th route.
func retainTx(ctx context.Context, tx *db.Tx, node string, bindings []Binding, candidates []int, workspaces map[string]*workspaceAdmission, errs []error) error {
	var write []int
	queued := map[string]bool{}
	for _, i := range candidates {
		b := bindings[i]
		key := bindingKey(b.ID, b.Environment, b.Route, b.Revision)
		if queued[key] {
			continue // the same binding twice in one group: one row, the first one's answer stands for both
		}
		queued[key] = true
		write = append(write, i)
	}
	if len(write) == 0 {
		return nil
	}
	n := len(write)
	var authority, environment, route, workspace, user []string
	var revision []int64
	var issued, expires, retain []time.Time
	authority, environment, route, workspace, user = make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	revision, issued, expires, retain = make([]int64, n), make([]time.Time, n), make([]time.Time, n), make([]time.Time, n)
	for k, i := range write {
		b := bindings[i]
		authority[k], environment[k], route[k], revision[k] = b.ID, b.Environment, b.Route, b.Revision
		workspace[k], user[k] = workspaces[b.Team+"\x00"+b.User].id, b.User
		issued[k], expires[k], retain[k] = b.IssuedAt, b.ExpiresAt, b.ExpiresAt.Add(2*DeliveryWindow)
	}
	written, err := tx.Query(ctx, `INSERT INTO usage_accounting_authorities(
 authority_id,edge_node_id,environment_id,route_id,route_revision,workspace_id,
 admitted_at,admission_expires_at,retain_until,actor_account_id
) SELECT a,$1,e,r,v,w,i,x,t,u FROM unnest($2::text[],$3::text[],$4::text[],$5::bigint[],$6::text[],$7::timestamptz[],$8::timestamptz[],$9::timestamptz[],$10::text[]) AS q(a,e,r,v,w,i,x,t,u)
ON CONFLICT(authority_id,edge_node_id,environment_id,route_id,route_revision)
DO UPDATE SET
 actor_account_id=EXCLUDED.actor_account_id,
 admission_expires_at=greatest(usage_accounting_authorities.admission_expires_at,EXCLUDED.admission_expires_at),
 retain_until=greatest(usage_accounting_authorities.retain_until,EXCLUDED.retain_until)
WHERE usage_accounting_authorities.workspace_id=EXCLUDED.workspace_id
 AND (usage_accounting_authorities.actor_account_id IS NULL
      OR usage_accounting_authorities.actor_account_id=EXCLUDED.actor_account_id)
RETURNING authority_id,environment_id,route_id,route_revision`, node, authority, environment, route, revision, workspace, issued, expires, retain, user)
	if err != nil {
		return err
	}
	stored := map[string]bool{}
	for written.Next() {
		var a, e, r string
		var v int64
		if err := written.Scan(&a, &e, &r, &v); err != nil {
			written.Close()
			return err
		}
		stored[bindingKey(a, e, r, v)] = true
	}
	written.Close()
	if err := written.Err(); err != nil {
		return err
	}
	// A row the conflict clause refused (another workspace or actor holds that authority) is not admitted.
	for _, i := range candidates {
		b := bindings[i]
		if !stored[bindingKey(b.ID, b.Environment, b.Route, b.Revision)] && (errs[i] == nil || errors.Is(errs[i], ErrThrottled)) {
			errs[i] = ErrUnavailable
		}
	}
	return nil
}
