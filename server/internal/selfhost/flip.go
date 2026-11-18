package selfhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/publicationdns"
)

// ErrLiveEdgeModeChange refuses a change of edge ownership while anything the edges serve is still running.
var ErrLiveEdgeModeChange = fmt.Errorf("%w: stop the workspace's running previews and tunnels before changing edge ownership; configure customer DNS and certificates before restarting them", ErrInvalid)

// LiveItem is one running preview or tunnel an edge ownership change would strand. Browser terminals are not
// listed: they always use Paperboat's edges, whatever the mode.
type LiveItem struct {
	Kind string `json:"kind"` // preview | tunnel
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LivePublicationsError is ErrLiveEdgeModeChange with how many things are still running and the first few by name.
type LivePublicationsError struct {
	Count int
	Items []LiveItem
}

func (e *LivePublicationsError) Error() string { return ErrLiveEdgeModeChange.Error() }
func (e *LivePublicationsError) Unwrap() error { return ErrLiveEdgeModeChange }

// liveEdgeSQL lists a workspace's running previews and tunnels (args: account, team or NULL): everything a
// change of edge ownership would strand.
const liveEdgeSQL = `SELECT kind,id,name,count(*) OVER () FROM (
 SELECT 'preview' AS kind,id,endpoint AS name FROM paperboat.preview_leases WHERE ($2::text IS NOT NULL OR account_id=$1) AND owner_team_id IS NOT DISTINCT FROM $2::text AND terminal_state='active' AND lease_deadline>now()
 UNION ALL SELECT 'tunnel',t.id,t.name FROM paperboat.tunnels t WHERE ($2::text IS NOT NULL OR t.account_id=$1) AND paperboat.publication_workspace_team('tunnel',t.id) IS NOT DISTINCT FROM $2::text AND desired_state='active' AND deleted_at IS NULL
) live ORDER BY kind,name,id LIMIT 50`

// refuseLiveEdgeChange returns a LivePublicationsError (the first 50 running things, and the full count) when the workspace still has running resources.
func refuseLiveEdgeChange(ctx context.Context, tx *db.Tx, account string, team any) error {
	rows, err := tx.Query(ctx, liveEdgeSQL, account, team)
	if err != nil {
		return err
	}
	defer rows.Close()
	live := &LivePublicationsError{}
	for rows.Next() {
		var item LiveItem
		if err = rows.Scan(&item.Kind, &item.ID, &item.Name, &live.Count); err != nil {
			return err
		}
		live.Items = append(live.Items, item)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if live.Count > 0 {
		return live
	}
	return nil
}

// workspaceResourcesSQL names a workspace's stopped-or-running previews and tunnels that still have domains
// (args: account, team or NULL): what a change of edge ownership must point at the new edges.
const workspaceResourcesSQL = `SELECT 'preview',l.id,l.endpoint FROM paperboat.preview_leases l WHERE ($2::text IS NOT NULL OR l.account_id=$1) AND l.owner_team_id IS NOT DISTINCT FROM $2::text AND l.terminal_state='active'
 AND EXISTS(SELECT 1 FROM paperboat.preview_domains d WHERE d.preview_id=l.id AND d.deleted_at IS NULL)
 UNION ALL SELECT 'tunnel',t.id,t.stable_endpoint FROM paperboat.tunnels t WHERE ($2::text IS NOT NULL OR t.account_id=$1) AND paperboat.publication_workspace_team('tunnel',t.id) IS NOT DISTINCT FROM $2::text AND t.deleted_at IS NULL
 AND EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.tunnel_id=t.id AND d.deleted_at IS NULL)`

// ErrNewEdgesNotAddressable means a workspace's domains cannot point at the new edges yet.
var ErrNewEdgesNotAddressable = fmt.Errorf("%w: your domains cannot point at the new servers yet because none has a verified public address; wait until a server shows as ready, then try again", ErrInvalid)

// repointWorkspaceDomains runs after a workspace's edge mode flips, in the flipping transaction: it drops the
// workspace's placements on servers of the old mode and points every domain of its previews and tunnels at the
// new edges (bumping the domain generation, so observations of the old target are ignored). Without it the
// resources would resume with records for edges that no longer serve them.
func repointWorkspaceDomains(ctx context.Context, tx *db.Tx, account string, team any, mode string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.selfhost_placements WHERE (resource_kind='preview' AND resource_id IN (SELECT id FROM paperboat.preview_leases WHERE ($2::text IS NOT NULL OR account_id=$1) AND owner_team_id IS NOT DISTINCT FROM $2::text))
 OR (resource_kind='tunnel' AND resource_id IN (SELECT t.id FROM paperboat.tunnels t WHERE ($2::text IS NOT NULL OR t.account_id=$1) AND paperboat.publication_workspace_team('tunnel',t.id) IS NOT DISTINCT FROM $2::text))`, account, team); err != nil {
		return err
	}
	type resource struct{ kind, id, endpoint string }
	rows, err := tx.Query(ctx, workspaceResourcesSQL, account, team)
	if err != nil {
		return err
	}
	var resources []resource
	for rows.Next() {
		var r resource
		if err = rows.Scan(&r.kind, &r.id, &r.endpoint); err != nil {
			rows.Close()
			return err
		}
		resources = append(resources, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, r := range resources {
		targets, resolveErr := publicationdns.ResolveTargetsTx(ctx, tx, r.kind, r.id, r.endpoint)
		if errors.Is(resolveErr, publicationdns.ErrSelfhostPlacementRequired) {
			return ErrNewEdgesNotAddressable
		}
		if resolveErr != nil {
			return resolveErr
		}
		host := ""
		if mode == "self-hosted-only" {
			host = targets[0]
		}
		if err = retargetDomains(ctx, tx, r.kind, r.id, r.endpoint, host, targets); err != nil {
			return err
		}
	}
	return nil
}

// retargetDomains points every domain of a preview or tunnel at targets (host is the one its dns_target names,
// empty for the managed name), resetting what was observed against the old target and bumping the generation
// that fences observations still in flight. A domain already at the target is left alone.
func retargetDomains(ctx context.Context, tx *db.Tx, kind, id, endpoint, host string, targets []string) error {
	target, err := publicationdns.Target(endpoint, host)
	if err != nil {
		return err
	}
	domainTable, resourceColumn := "paperboat.preview_domains", "preview_id"
	if kind == "tunnel" {
		domainTable, resourceColumn = "paperboat.tunnel_domains", "tunnel_id"
	}
	type domain struct{ id, hostname, provider string }
	rows, err := tx.Query(ctx, `SELECT id,hostname,dns_provider FROM `+domainTable+` WHERE `+resourceColumn+`=$1 AND deleted_at IS NULL AND dns_target<>$2 FOR UPDATE`, id, target)
	if err != nil {
		return err
	}
	var domains []domain
	for rows.Next() {
		var d domain
		if err = rows.Scan(&d.id, &d.hostname, &d.provider); err != nil {
			rows.Close()
			return err
		}
		domains = append(domains, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(targets) == 0 {
		targets = []string{target}
	}
	for _, d := range domains {
		records, marshalErr := json.Marshal(publicationdns.Records(targets, d.hostname, d.provider))
		if marshalErr != nil {
			return marshalErr
		}
		// Ownership comes from the hostname's workspace domain, so a move keeps it verified.
		if _, err = tx.Exec(ctx, `UPDATE `+domainTable+` SET dns_target=$2,expected_records=$3::jsonb,
            conflict_state='clear',quarantine_until=NULL,observed_records='[]'::jsonb,
            generation=generation+1,updated_at=now() WHERE id=$1`, d.id, target, records); err != nil {
			return err
		}
	}
	return nil
}
