package browseraccess

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type requestTarget struct {
	kind, id, route string
}

// returnRequestPath follows the edge route matcher's path rules. Begin receives
// the edge's original request URI, including its query, while routes match the
// decoded URL.Path only.
func returnRequestPath(returnPath string) (string, error) {
	u, err := url.ParseRequestURI(returnPath)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/") || strings.ContainsAny(u.Path, "\r\n\x00?#") || strings.Contains(u.Path, "//") {
		return "", ErrInvalid
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", ErrInvalid
		}
	}
	if len(u.Path) > 1 {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
	return u.Path, nil
}

func resolveBeginTarget(ctx context.Context, tx *db.Tx, host, path string, now time.Time, machineServiceDomain string) (requestTarget, error) {
	if a, err := resolveResource(ctx, tx, host, "lazy_policy", "", "", now); err == nil {
		return requestTarget{kind: a.kind, id: a.id, route: a.route}, nil
	} else if err != ErrNotAuthorized {
		return requestTarget{}, err
	}
	var reserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lazy_access_policies WHERE hostname=$1)`, host).Scan(&reserved); err != nil {
		return requestTarget{}, err
	}
	if reserved {
		return requestTarget{}, ErrNotAuthorized
	}
	if a, err := resolveResource(ctx, tx, host, "preview", "", "", now); err == nil {
		return requestTarget{kind: a.kind, id: a.id, route: a.route}, nil
	} else if err != ErrNotAuthorized {
		return requestTarget{}, err
	}
	if a, err := resolveTunnelRequest(ctx, tx, host, path, now); err == nil {
		return requestTarget{kind: a.kind, id: a.id, route: a.route}, nil
	} else if err != ErrNotAuthorized {
		return requestTarget{}, err
	}
	// Machine-service lazy policies are created only after a browser identity is
	// known. They remain a legacy owner-only flow, outside preview/tunnel policy.
	if machineServiceDomain != "" && (host == machineServiceDomain || strings.HasSuffix(host, "."+machineServiceDomain)) {
		return requestTarget{kind: "machine_service_pending", id: host, route: host}, nil
	}
	return requestTarget{}, ErrNotAuthorized
}

type routeCandidate struct {
	authority
	hostClass, suffixLabels, pathLength, priority int
}

func resolveTunnelRequest(ctx context.Context, tx *db.Tx, host, path string, now time.Time) (authority, error) {
	rows, err := tx.Query(ctx, `
SELECT t.id,t.account_id,t.access_mode,t.generation,r.id,r.generation,
       COALESCE(t.expires_at,$2::timestamptz+interval '100 years'),t.created_by_host_id,
       t.stable_endpoint,r.match_type,r.match_hostname,r.wildcard_suffix,r.path_prefix,r.priority,
       COALESCE(d.match_type,''),COALESCE(d.hostname,'')
FROM tunnels t
JOIN tunnel_routes r ON r.tunnel_id=t.id
LEFT JOIN tunnel_domains d ON d.route_id=r.id AND d.tunnel_id=t.id
 AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear'
 AND ((r.protocol='tls' AND d.certificate_strategy='none' AND d.certificate_state='not_applicable' AND d.match_type='exact')
   OR (r.protocol<>'tls' AND d.certificate_state='ready'))
WHERE t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$2)
 AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol='http'
 AND ((t.stable_endpoint='https://'||$1)
   OR r.match_hostname=$1
   OR (r.match_type='one_label_wildcard' AND $1 LIKE '%.'||r.wildcard_suffix)
   OR r.match_type='catch_all'
   OR d.hostname IS NOT NULL)
	ORDER BY r.id,d.hostname`, host, now)
	if err != nil {
		return authority{}, err
	}
	defer rows.Close()
	var candidates []routeCandidate
	for rows.Next() {
		var a authority
		var matchType, stableEndpoint string
		var exact, suffix, pathPrefix, domainMatchType, domainHostname *string
		var priority int32
		if err := rows.Scan(&a.id, &a.owner, &a.mode, &a.generation, &a.route, &a.routeGeneration, &a.expires, &a.machine,
			&stableEndpoint, &matchType, &exact, &suffix, &pathPrefix, &priority, &domainMatchType, &domainHostname); err != nil {
			return authority{}, err
		}
		a.nativeMode = a.mode
		if pathPrefix != nil && *pathPrefix != "" && *pathPrefix != "/" && path != *pathPrefix && !strings.HasPrefix(path, *pathPrefix+"/") {
			continue
		}
		pathLen := 0
		if pathPrefix != nil {
			pathLen = len(*pathPrefix)
		}
		class, labels := 0, 0
		if domainMatchType != nil && domainHostname != nil {
			class, labels = hostnameMatch(*domainMatchType, *domainHostname, host)
		}
		if class == 0 {
			switch matchType {
			case "managed", "exact":
				if exact != nil && *exact == host {
					class = 3
				}
			case "one_label_wildcard":
				if suffix != nil {
					class, labels = hostnameMatch("one_label_wildcard", "*."+*suffix, host)
				}
			case "catch_all":
				if stableEndpoint == "https://"+host {
					class = 3
				}
			}
		}
		// A managed stable hostname is an exact route host even when an older
		// row predates match_hostname population.
		if class == 0 && stableEndpoint == "https://"+host {
			class = 3
		}
		if class == 0 {
			continue
		}
		a.kind = "tunnel"
		candidates = append(candidates, routeCandidate{authority: a, hostClass: class, suffixLabels: labels, pathLength: pathLen, priority: int(priority)})
	}
	if err := rows.Err(); err != nil {
		return authority{}, err
	}
	if len(candidates) == 0 {
		return authority{}, ErrNotAuthorized
	}
	best := candidates[0]
	ambiguous := false
	for _, candidate := range candidates[1:] {
		cmp := compareRouteCandidate(candidate, best)
		if cmp > 0 {
			best, ambiguous = candidate, false
		} else if cmp == 0 && candidate.route != best.route {
			ambiguous = true
		}
	}
	if ambiguous {
		return authority{}, ErrNotAuthorized
	}
	return best.authority, nil
}

func hostnameMatch(kind, rule, host string) (int, int) {
	switch kind {
	case "exact", "managed":
		if rule == host {
			return 3, 0
		}
	case "one_label_wildcard":
		suffix := strings.TrimPrefix(rule, "*.")
		if suffix != rule && strings.HasSuffix(host, "."+suffix) {
			prefix := strings.TrimSuffix(host, "."+suffix)
			if prefix != "" && !strings.Contains(prefix, ".") {
				return 2, strings.Count(suffix, ".") + 1
			}
		}
	}
	return 0, 0
}

func compareRouteCandidate(left, right routeCandidate) int {
	for _, pair := range [][2]int{{left.hostClass, right.hostClass}, {left.suffixLabels, right.suffixLabels}, {left.pathLength, right.pathLength}} {
		if pair[0] > pair[1] {
			return 1
		}
		if pair[0] < pair[1] {
			return -1
		}
	}
	if left.priority < right.priority {
		return 1
	}
	if left.priority > right.priority {
		return -1
	}
	return 0
}

func resolveBrowserBound(ctx context.Context, tx *db.Tx, account, host, kind, id, route string, now time.Time) (authority, error) {
	a, err := resolveResource(ctx, tx, host, kind, id, route, now)
	if err != nil {
		return a, err
	}
	if kind == "preview" || kind == "tunnel" {
		return applyViewerPolicy(ctx, tx, a, account)
	}
	return applyPrincipal(ctx, tx, a, account)
}
