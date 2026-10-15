// Package publicationdns derives custom-domain records from the resource's edge placement.
package publicationdns

import (
	"context"
	"errors"
	"golang.org/x/net/publicsuffix"
	"net"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// ResolveTargetsTx returns the complete DNS address set for at most two selected
// self-hosted edges. Observed ingress addresses, rather than operator claims,
// own public DNS. The resource lock fences this selection against mode changes.
func ResolveTargetsTx(ctx context.Context, tx *db.Tx, kind, id, endpoint string) ([]string, error) {
	var mode string
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode(account_id,paperboat.publication_workspace_team($1,$2)) FROM (SELECT account_id FROM paperboat.preview_leases WHERE $1='preview' AND id=$2 UNION ALL SELECT account_id FROM paperboat.tunnels WHERE $1='tunnel' AND id=$2) resource`, kind, id).Scan(&mode); err != nil {
		return nil, err
	}
	if mode == "managed-only" {
		var forced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_placements WHERE resource_kind=$1 AND resource_id=$2)`, kind, id).Scan(&forced); err != nil {
			return nil, err
		}
		if forced {
			return nil, ErrPlacementModeMismatch
		}
		target, err := Target(endpoint, "")
		if err != nil {
			return nil, err
		}
		return []string{target}, nil
	}
	rows, err := tx.Query(ctx, `SELECT coalesce(host(n.public_ingress_ipv4),''),coalesce(host(n.public_ingress_ipv6),'') FROM paperboat.publication_edge_nodes($1,$2) selected JOIN paperboat.control_tunnel_nodes n ON n.id=selected.node_id ORDER BY n.id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var v4, v6 string
		if err := rows.Scan(&v4, &v6); err != nil {
			return nil, err
		}
		for _, value := range []string{v4, v6} {
			if value != "" && !seen[value] {
				targets = append(targets, value)
				seen[value] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, ErrSelfhostPlacementRequired
	}
	return targets, nil
}

type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

func Records(targets []string, hostname, provider string) []Record {
	records := make([]Record, 0, len(targets))
	for _, target := range targets {
		records = append(records, Record{hostname, DomainRecordType(target, hostname, provider), target, 300})
	}
	return records
}

var ErrSelfhostPlacementRequired = errors.New("self-hosted DNS target is unavailable; select eligible servers with verified public ingress before configuring the domain")
var ErrPlacementModeMismatch = errors.New("publication placement does not match workspace edge mode; update its placement")

type targetParseError struct{ cause error }

func (e targetParseError) Error() string { return "publication DNS target is unavailable" }
func (e targetParseError) Unwrap() error { return e.cause }

func Target(endpoint, selected string) (string, error) {
	host := strings.TrimSpace(selected)
	if host == "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return "", targetParseError{cause: err}
		}
		if parsed.Hostname() == "" {
			return "", errors.New("publication DNS target is unavailable")
		}
		host = parsed.Hostname()
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if host == "" || strings.ContainsAny(host, "/: \t\r\n") {
		return "", errors.New("publication DNS target is invalid")
	}
	return strings.ToLower(host), nil
}

func RecordType(target, hostnameRecordType string) string {
	if ip := net.ParseIP(target); ip != nil {
		if ip.To4() != nil {
			return "A"
		}
		return "AAAA"
	}
	return hostnameRecordType
}

func Note(target, hostnameNote string) string {
	if net.ParseIP(target) != nil {
		return "Add the shown address record for the selected server. Paperboat waits for DNS verification before TLS becomes ready."
	}
	return hostnameNote
}

// DomainRecordType preserves provider-specific apex aliases for hostname targets.
func DomainRecordType(target, hostname, provider string) string {
	hostnameType := "CNAME"
	base := strings.TrimPrefix(hostname, "*.")
	registrable, err := publicsuffix.EffectiveTLDPlusOne(base)
	if err == nil && registrable == base && !strings.HasPrefix(hostname, "*.") {
		hostnameType = "ANAME"
		if provider == "route53" {
			hostnameType = "ALIAS"
		}
		if provider == "cloudflare" {
			hostnameType = "CNAME"
		}
	}
	return RecordType(target, hostnameType)
}
