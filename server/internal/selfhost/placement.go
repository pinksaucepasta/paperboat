package selfhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/publicationdns"
)

var ErrPlacementMode = fmt.Errorf("%w: placement must match the workspace edge mode", ErrInvalid)
var ErrPlacementDomainRequired = fmt.Errorf("%w: verify a custom domain before placing a publication on a self-hosted server", ErrInvalid)

type Placement struct {
	InstallationID *string `json:"installation_id"`
	NodeID         *string `json:"node_id"`
}

func resourceTable(kind string) (string, error) {
	switch kind {
	case "preview":
		return "paperboat.preview_leases", nil
	case "tunnel":
		return "paperboat.tunnels", nil
	}
	return "", ErrInvalid
}
func (s *Service) Placement(ctx context.Context, actor, kind, id string) (Placement, error) {
	table, err := resourceTable(kind)
	if err != nil {
		return Placement{}, err
	}
	var exists bool
	if err = s.database.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1 AND account_id=$2)`, id, actor).Scan(&exists); err != nil {
		return Placement{}, err
	}
	if !exists {
		return Placement{}, ErrUnauthorized
	}
	var installation, node string
	err = s.database.SQL().QueryRowContext(ctx, `SELECT p.installation_id,i.node_id FROM paperboat.selfhost_placements p JOIN paperboat.selfhost_installations i ON i.id=p.installation_id WHERE p.resource_kind=$1 AND p.resource_id=$2`, kind, id).Scan(&installation, &node)
	if err == sql.ErrNoRows {
		return Placement{}, nil
	}
	if err != nil {
		return Placement{}, err
	}
	return Placement{&installation, &node}, nil
}
func (s *Service) SetPlacement(ctx context.Context, actor, kind, id string, installation *string) (result Placement, returnErr error) {
	table, err := resourceTable(kind)
	if err != nil {
		return Placement{}, err
	}
	tx, err := s.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return Placement{}, err
	}
	defer func() { returnErr = withTransactionRollbackFailure("set_placement", returnErr, tx.Rollback()) }()
	var owner, endpoint string
	endpointColumn := "endpoint"
	if kind == "tunnel" {
		endpointColumn = "stable_endpoint"
	}
	err = tx.QueryRowContext(ctx, `SELECT account_id,`+endpointColumn+` FROM `+table+` WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &endpoint)
	if err == sql.ErrNoRows || err == nil && owner != actor {
		return Placement{}, ErrUnauthorized
	}
	if err != nil {
		return Placement{}, err
	}
	var mode string
	var team sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT paperboat.publication_workspace_team($1,$2),paperboat.workspace_edge_mode($3,paperboat.publication_workspace_team($1,$2))`, kind, id, owner).Scan(&team, &mode); err != nil {
		return Placement{}, err
	}
	if (installation == nil && mode != "managed-only") || (installation != nil && mode != "self-hosted-only") {
		return Placement{}, ErrPlacementMode
	}
	if installation != nil {
		domainTable, resourceColumn := "paperboat.preview_domains", "preview_id"
		if kind == "tunnel" {
			domainTable, resourceColumn = "paperboat.tunnel_domains", "tunnel_id"
		}
		var verified bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+domainTable+` WHERE `+resourceColumn+`=$1 AND deleted_at IS NULL AND ownership_state='verified' AND conflict_state='clear')`, id).Scan(&verified); err != nil {
			return Placement{}, err
		}
		if !verified {
			return Placement{}, ErrPlacementDomainRequired
		}
	}
	out := Placement{}
	selectedHost := ""
	var selectedV4, selectedV6 string
	if installation == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM paperboat.selfhost_placements WHERE resource_kind=$1 AND resource_id=$2`, kind, id)
	} else {
		var node string
		err = tx.QueryRowContext(ctx, `SELECT i.node_id,n.endpoint_host,coalesce(host(n.public_ingress_ipv4),''),coalesce(host(n.public_ingress_ipv6),'') FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.id=$1 AND i.capability='tunnel' AND i.state='active' AND i.claim_complete AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$2) AND paperboat.node_workspace_edge_eligible(i.node_id,$3,$4) FOR SHARE OF i,n`, *installation, actor, owner, team).Scan(&node, &selectedHost, &selectedV4, &selectedV6)
		if err == sql.ErrNoRows {
			return out, ErrUnauthorized
		}
		if err != nil {
			return out, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO paperboat.selfhost_placements(resource_kind,resource_id,installation_id) VALUES($1,$2,$3) ON CONFLICT(resource_kind,resource_id) DO UPDATE SET installation_id=excluded.installation_id`, kind, id, *installation)
		out = Placement{installation, &node}
	}
	if err != nil {
		return out, err
	}
	targets := []string{}
	for _, value := range []string{selectedV4, selectedV6} {
		if value != "" {
			targets = append(targets, value)
		}
	}
	if len(targets) > 0 {
		selectedHost = targets[0]
	}
	target, err := publicationdns.Target(endpoint, selectedHost)
	if err != nil {
		return out, err
	}
	domainTable, resourceColumn := "paperboat.preview_domains", "preview_id"
	if kind == "tunnel" {
		domainTable, resourceColumn = "paperboat.tunnel_domains", "tunnel_id"
	}
	// The resource lock serializes domain creation and placement changes. Domain
	// generations fence observations already in flight against the old target.
	rows, err := tx.QueryContext(ctx, `SELECT id,hostname,dns_provider FROM `+domainTable+` WHERE `+resourceColumn+`=$1 AND deleted_at IS NULL AND dns_target<>$2 FOR UPDATE`, id, target)
	if err != nil {
		return out, err
	}
	type domainTarget struct{ id, hostname, provider string }
	var domains []domainTarget
	for rows.Next() {
		var domain domainTarget
		if err = rows.Scan(&domain.id, &domain.hostname, &domain.provider); err != nil {
			return out, withDatabaseCleanupFailure("set_placement", err, rows.Close())
		}
		domains = append(domains, domain)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	err = withDatabaseCleanupFailure("set_placement", rowsErr, closeErr)
	if err != nil {
		return out, err
	}
	for _, domain := range domains {
		if len(targets) == 0 {
			targets = []string{target}
		}
		records, marshalErr := json.Marshal(publicationdns.Records(targets, domain.hostname, domain.provider))
		if marshalErr != nil {
			return out, marshalErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE `+domainTable+` SET dns_target=$2,expected_records=$3::jsonb,
            ownership_state='pending',conflict_state='clear',quarantine_until=NULL,observed_records='[]'::jsonb,
            verification_attempts=0,dns_last_checked_at=NULL,dns_next_check_at=now(),dns_ttl_seconds=NULL,last_verified_at=NULL,
            generation=generation+1,updated_at=now() WHERE id=$1`, domain.id, target, records)
		if err != nil {
			return out, err
		}
	}

	// A tunnel needs no edge update: its edge snapshot filters on node_route_eligible, so the move takes effect on the next pull.
	if kind != "tunnel" {
		if _, err = tx.ExecContext(ctx, `UPDATE paperboat.preview_leases SET edge_state='pending' WHERE id=$1`, id); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}
