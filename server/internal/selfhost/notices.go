package selfhost

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ReleaseComponent is the release component an installation's capability runs: "edge" for a tunnel server, "relay" for a relay.
func ReleaseComponent(capability string) string {
	switch capability {
	case "tunnel":
		return "edge"
	case "relay":
		return "relay"
	}
	return ""
}

// noticeLifetime is how long a node notice stays in the bell (announcements retention removes it after).
const noticeLifetime = 30 * 24 * time.Hour

// notify puts a notice in the bell of whoever owns node's installations: the account (personal), the team's owner
// (team), or operators in admin (global nodes). One notice per key, ever: a repeat is a no-op.
func (s *Service) notify(ctx context.Context, node, key, severity, title, body string) error {
	_, err := s.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.announcements(id,kind,title,body,link_url,severity,dismissible,audience_kind,audience_value,starts_at,ends_at,created_by,idempotency_key)
SELECT $1,'notice',$2,$3,CASE WHEN i.scope_kind='global' THEN NULL ELSE '/network' END,$4,true,
 CASE WHEN i.scope_kind='global' THEN 'operators' ELSE 'user' END,
 CASE i.scope_kind WHEN 'global' THEN '' WHEN 'team' THEN (SELECT t.owner_account FROM paperboat.teams t WHERE t.team_id=i.scope_id) ELSE i.scope_id END,
 $5,$6,'system',$7
FROM paperboat.selfhost_installations i WHERE i.node_id=$8 AND i.state='active' ORDER BY i.created_at LIMIT 1
ON CONFLICT (created_by,idempotency_key) DO NOTHING`, "ann_"+uuid.NewString(), clip(title, 80), clip(body, 280), severity, s.now().UTC(), s.now().UTC().Add(noticeLifetime), key, node)
	return err
}

// clip shortens text to at most n characters (announcements' limits), on a rune boundary.
func clip(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n-1]) + "…"
}

// NodeUpdateState answers, for a component at version, the newer published release (available) and, below the
// floor, why and how to update (required); controlplane.NodeUpdateState with the app's floors.
type NodeUpdateState func(ctx context.Context, component, version string) (available, required string)

// NotifyNodeUpdates tells each self-hosted node's owner, once per release, that it is outdated or below the update
// floor. Operators get the same for global nodes.
func (s *Service) NotifyNodeUpdates(ctx context.Context, state NodeUpdateState) error {
	rows, err := s.database.SQL().QueryContext(ctx, `SELECT DISTINCT ON (i.node_id,i.capability) i.node_id,i.capability,i.name,c.version
FROM paperboat.selfhost_installations i JOIN paperboat.node_capacity c ON c.node_id=i.node_id
WHERE i.state='active' AND i.claim_complete AND c.version<>'' ORDER BY i.node_id,i.capability,i.created_at`)
	if err != nil {
		return err
	}
	type node struct{ id, capability, name, version string }
	var nodes []node
	for rows.Next() {
		var n node
		if err = rows.Scan(&n.id, &n.capability, &n.name, &n.version); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, n := range nodes {
		component := ReleaseComponent(n.capability)
		available, required := state(ctx, component, n.version)
		switch {
		case required != "":
			err = s.notify(ctx, n.id, "node-update-required:"+n.id+":"+component+":"+n.version, "warning", "Update required: "+n.name, required)
		case available != "":
			err = s.notify(ctx, n.id, "node-update-available:"+n.id+":"+component+":"+available, "info", "Update available: "+n.name,
				fmt.Sprintf("The %s on %s runs %s; %s is published. Run the pbh install command on the host again to update it.", component, n.name, n.version, available))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// notifyUpdateFinished tells the owner how a requested update ended: succeeded, rolled back, or failed with why.
func (s *Service) notifyUpdateFinished(ctx context.Context, node, requestID, state, target, detail, reference string) error {
	var name string
	if err := s.database.SQL().QueryRowContext(ctx, `SELECT name FROM paperboat.selfhost_installations WHERE node_id=$1 AND state='active' ORDER BY created_at LIMIT 1`, node).Scan(&name); err != nil {
		return err
	}
	severity, title, body := "info", "Updated: "+name, name+" now runs "+target+"."
	if state != "succeeded" {
		severity, title = "warning", "Update failed: "+name
		body = name + " did not update to " + target + ": " + detail
		if state == "rolled_back" {
			body += " It rolled back and runs its previous release."
		}
		if reference != "" {
			body += " Support reference " + reference + "."
		}
	}
	return s.notify(ctx, node, "node-update:"+requestID+":"+state, severity, title, body)
}
