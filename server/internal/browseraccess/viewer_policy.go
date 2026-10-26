package browseraccess

import (
	"context"
	"errors"
	"net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"golang.org/x/net/idna"
)

const (
	AudienceOwner          = "owner"
	AudienceTeam           = "team"
	AudienceSpecificEmails = "specific_emails"
	AudienceEmailDomain    = "email_domain"
	AudiencePublic         = "public"
)

var ErrPolicyConflict = errors.New("viewer policy generation conflict")
var ErrPolicyOwnerChanged = errors.New("viewer policy owner changed")

type ViewerPolicy struct {
	ResourceKind     string    `json:"resource_kind"`
	ResourceID       string    `json:"resource_id"`
	RouteID          string    `json:"route_id"`
	OwnerAccountID   string    `json:"owner_account_id"`
	Audience         string    `json:"audience,omitempty"`
	TeamID           string    `json:"team_id,omitempty"`
	AllowedEmails    []string  `json:"allowed_emails,omitempty"`
	EmailDomain      string    `json:"email_domain,omitempty"`
	Generation       int64     `json:"generation"`
	Configured       bool      `json:"configured"`
	NativeAccessMode string    `json:"native_access_mode"`
	ServingState     string    `json:"serving_state"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

type SetViewerPolicyRequest struct {
	ActorAccountID     string
	ResourceKind       string
	ResourceID         string
	RouteID            string
	Audience           string
	TeamID             string
	AllowedEmails      []string
	EmailDomain        string
	ExpectedGeneration int64
}

type policyTarget struct {
	owner, mode                         string
	resourceGeneration, routeGeneration int64
	expires                             time.Time
}

func (s *Service) GetViewerPolicy(ctx context.Context, actor, kind, id, route string) (ViewerPolicy, error) {
	if actor == "" || !validPolicyResource(kind, id, route) {
		return ViewerPolicy{}, ErrInvalid
	}
	var out ViewerPolicy
	now := s.now()
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		target, err := loadPolicyTarget(ctx, tx, kind, id, route, now)
		if err != nil {
			return err
		}
		if err = requirePolicyManager(ctx, tx, actor, kind, id, target.owner); err != nil {
			return err
		}
		out = ViewerPolicy{ResourceKind: kind, ResourceID: id, RouteID: route, OwnerAccountID: target.owner, NativeAccessMode: target.mode}
		if err = readViewerPolicy(ctx, tx, &out); err != nil {
			return err
		}
		return setViewerPolicyServingState(ctx, tx, &out, now)
	})
	return out, err
}

func (s *Service) SetViewerPolicy(ctx context.Context, in SetViewerPolicyRequest) (ViewerPolicy, error) {
	if in.ActorAccountID == "" || !validPolicyResource(in.ResourceKind, in.ResourceID, in.RouteID) || in.ExpectedGeneration < 0 {
		return ViewerPolicy{}, ErrInvalid
	}
	policy, err := normalizeViewerPolicy(in)
	if err != nil {
		return ViewerPolicy{}, err
	}
	now := s.now()
	var out ViewerPolicy
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		target, err := loadPolicyTarget(ctx, tx, in.ResourceKind, in.ResourceID, in.RouteID, now)
		if err != nil {
			return err
		}
		if err = requirePolicyManager(ctx, tx, in.ActorAccountID, in.ResourceKind, in.ResourceID, target.owner); err != nil {
			return err
		}
		if policy.Audience == AudienceTeam {
			if err = requireBoundPolicyTeam(ctx, tx, in.ActorAccountID, in.ResourceKind, in.ResourceID, target.owner, policy.TeamID); err != nil {
				return err
			}
		}
		if err = ensurePolicyEdgeMode(ctx, tx, in.ResourceKind, in.RouteID, target.mode, policy.Audience); err != nil {
			return err
		}
		var generation int64
		var policyOwner string
		err = tx.QueryRow(ctx, `SELECT generation,owner_account_id FROM browser_viewer_policies WHERE resource_kind=$1 AND resource_id=$2 AND route_id=$3 FOR UPDATE`, in.ResourceKind, in.ResourceID, in.RouteID).Scan(&generation, &policyOwner)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == pgx.ErrNoRows {
			generation = 0
		}
		if policyOwner != "" && policyOwner != target.owner {
			return ErrPolicyOwnerChanged
		}
		if generation != in.ExpectedGeneration {
			return ErrPolicyConflict
		}
		if generation >= 9007199254740991 {
			return ErrCapacity
		}
		generation++
		_, err = tx.Exec(ctx, `
INSERT INTO browser_viewer_policies(resource_kind,resource_id,route_id,owner_account_id,audience,team_id,allowed_emails,email_domain,generation,updated_by_account,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
ON CONFLICT(resource_kind,resource_id,route_id) DO UPDATE SET
 owner_account_id=EXCLUDED.owner_account_id,audience=EXCLUDED.audience,team_id=EXCLUDED.team_id,
 allowed_emails=EXCLUDED.allowed_emails,email_domain=EXCLUDED.email_domain,generation=EXCLUDED.generation,
 updated_by_account=EXCLUDED.updated_by_account,updated_at=EXCLUDED.updated_at`,
			in.ResourceKind, in.ResourceID, in.RouteID, target.owner, policy.Audience,
			nullString(policy.TeamID), policy.AllowedEmails, nullString(policy.EmailDomain), generation, in.ActorAccountID, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE browser_access_sessions SET revoked_at=$4 WHERE resource_kind=$1 AND resource_id=$2 AND route_id=$3 AND revoked_at IS NULL`, in.ResourceKind, in.ResourceID, in.RouteID, now)
		if err != nil {
			return err
		}
		out = ViewerPolicy{ResourceKind: in.ResourceKind, ResourceID: in.ResourceID, RouteID: in.RouteID, OwnerAccountID: target.owner, NativeAccessMode: target.mode}
		if err = readViewerPolicy(ctx, tx, &out); err != nil {
			return err
		}
		return setViewerPolicyServingState(ctx, tx, &out, now)
	})
	return out, err
}

func setViewerPolicyServingState(ctx context.Context, tx *db.Tx, policy *ViewerPolicy, now time.Time) error {
	if policy.ResourceKind != "tunnel" {
		policy.ServingState = "not_applicable"
		return nil
	}
	var effectiveMode string
	var effectiveGeneration int64
	var readyAssignments, oldPublicWithinGrantLifetime int64
	err := tx.QueryRow(ctx, `
SELECT current.effective_access_mode,current.viewer_policy_generation,
       count(x.route_id) FILTER (WHERE x.desired='active' AND x.observed_ready),
       -- Tunnels ride the endpoint engine (PLAN 3a): an edge's public decision lives at most 10 s,
       -- so a served route made private or team is fenced for 10 s after the change.
       count(x.route_id) FILTER (WHERE x.desired='active' AND current.effective_access_mode IN ('private','team')
         AND COALESCE(viewer.updated_at,t.updated_at)+interval '10 seconds' > $4)
FROM tunnels t
JOIN tunnel_routes r ON r.tunnel_id=t.id
JOIN current_tunnel_route_viewer_policy current ON current.route_id=r.id
LEFT JOIN browser_viewer_policies viewer
  ON viewer.resource_kind='tunnel' AND viewer.resource_id=t.id AND viewer.route_id=r.id
 AND viewer.owner_account_id=t.account_id
LEFT JOIN endpoint_routes er ON er.tunnel_route_id=r.id
LEFT JOIN endpoint_edge_assignments x ON x.route_id=er.id
WHERE t.id=$1 AND r.id=$2 AND t.account_id=$3
GROUP BY current.effective_access_mode,current.viewer_policy_generation,viewer.updated_at,t.updated_at`,
		policy.ResourceID, policy.RouteID, policy.OwnerAccountID, now).Scan(&effectiveMode, &effectiveGeneration, &readyAssignments, &oldPublicWithinGrantLifetime)
	if err != nil {
		return mapMissing(err)
	}
	if effectiveGeneration < 1 || (effectiveMode != "public" && effectiveMode != "private" && effectiveMode != "team") {
		return ErrNotAuthorized
	}
	switch {
	case oldPublicWithinGrantLifetime > 0:
		policy.ServingState = "fencing"
	case readyAssignments > 0:
		policy.ServingState = "ready"
	default:
		policy.ServingState = "pending"
	}
	return nil
}

func validPolicyResource(kind, id, route string) bool {
	return (kind == "preview" || kind == "tunnel") && id != "" && len(id) <= 128 && route != "" && len(route) <= 128 && strings.TrimSpace(id) == id && strings.TrimSpace(route) == route
}

func normalizeViewerPolicy(in SetViewerPolicyRequest) (ViewerPolicy, error) {
	// PostgreSQL stores allowed_emails as a non-null array. Keep the canonical
	// empty value distinct from NULL for audiences that do not use email lists.
	p := ViewerPolicy{ResourceKind: in.ResourceKind, ResourceID: in.ResourceID, RouteID: in.RouteID, Audience: in.Audience, AllowedEmails: []string{}}
	switch in.Audience {
	case AudienceOwner, AudiencePublic:
		if in.TeamID != "" || len(in.AllowedEmails) != 0 || in.EmailDomain != "" {
			return ViewerPolicy{}, ErrInvalid
		}
	case AudienceTeam:
		if in.TeamID == "" || strings.TrimSpace(in.TeamID) != in.TeamID || len(in.TeamID) > 128 || len(in.AllowedEmails) != 0 || in.EmailDomain != "" {
			return ViewerPolicy{}, ErrInvalid
		}
		p.TeamID = in.TeamID
	case AudienceSpecificEmails:
		if in.TeamID != "" || in.EmailDomain != "" || len(in.AllowedEmails) == 0 || len(in.AllowedEmails) > 100 {
			return ViewerPolicy{}, ErrInvalid
		}
		seen := make(map[string]struct{}, len(in.AllowedEmails))
		for _, raw := range in.AllowedEmails {
			address, ok := canonicalEmail(raw)
			if !ok {
				return ViewerPolicy{}, ErrInvalid
			}
			if _, exists := seen[address]; exists {
				return ViewerPolicy{}, ErrInvalid
			}
			seen[address] = struct{}{}
			p.AllowedEmails = append(p.AllowedEmails, address)
		}
	case AudienceEmailDomain:
		if in.TeamID != "" || len(in.AllowedEmails) != 0 {
			return ViewerPolicy{}, ErrInvalid
		}
		domain, ok := canonicalDomain(in.EmailDomain)
		if !ok {
			return ViewerPolicy{}, ErrInvalid
		}
		p.EmailDomain = domain
	default:
		return ViewerPolicy{}, ErrInvalid
	}
	return p, nil
}

func canonicalEmail(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > 254 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "*") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return "", false
		}
	}
	parsed, err := mail.ParseAddress(raw)
	if err != nil || parsed.Address != raw || strings.Count(raw, "@") != 1 {
		return "", false
	}
	local, domain, found := strings.Cut(raw, "@")
	if !found || local == "" {
		return "", false
	}
	for _, r := range local {
		if r > 0x7f {
			return "", false
		}
	}
	canonical, ok := canonicalDomain(domain)
	if !ok {
		return "", false
	}
	return strings.ToLower(local) + "@" + canonical, true
}

func canonicalDomain(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "*/\\:@%?#\r\n\t ") || strings.HasSuffix(raw, ".") || !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if r == 0x7f || r < 0x20 {
			return "", false
		}
	}
	lower := strings.ToLower(raw)
	ascii, err := idna.Lookup.ToASCII(lower)
	if err != nil || ascii == "" || len(ascii) > 253 || strings.Contains(ascii, "..") {
		return "", false
	}
	ascii = strings.ToLower(ascii)
	if _, err := netip.ParseAddr(ascii); err == nil {
		return "", false
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
	}
	return ascii, true
}

func loadPolicyTarget(ctx context.Context, tx *db.Tx, kind, id, route string, now time.Time) (policyTarget, error) {
	var target policyTarget
	var err error
	switch kind {
	case "tunnel":
		err = tx.QueryRow(ctx, `SELECT t.account_id,t.access_mode,t.generation,r.generation,COALESCE(t.expires_at,$3::timestamptz+interval '100 years') FROM tunnels t JOIN tunnel_routes r ON r.tunnel_id=t.id WHERE t.id=$1 AND r.id=$2 AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$3) AND r.desired_state='active' AND r.deleted_at IS NULL AND r.protocol='http' FOR UPDATE OF t,r`, id, route, now).Scan(&target.owner, &target.mode, &target.resourceGeneration, &target.routeGeneration, &target.expires)
	case "preview":
		err = tx.QueryRow(ctx, `SELECT p.account_id,p.access_mode,p.generation,x.route_generation,LEAST(p.lease_deadline,COALESCE(p.user_deadline,p.lease_deadline),x.expires_at) FROM preview_leases p JOIN preview_routes x ON x.preview_id=p.id AND x.account_id=p.account_id WHERE p.id=$1 AND x.route_id=$2 AND p.terminal_state='active' AND p.lease_deadline>$3 AND x.state='ready' AND x.expires_at>$3 FOR UPDATE OF p`, id, route, now).Scan(&target.owner, &target.mode, &target.resourceGeneration, &target.routeGeneration, &target.expires)
	default:
		return target, ErrInvalid
	}
	if err != nil {
		return target, mapMissing(err)
	}
	return target, nil
}

func requirePolicyManager(ctx context.Context, tx *db.Tx, actor, kind, id, owner string) error {
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM users actor JOIN users owner ON owner.id=$3
 WHERE actor.id=$1 AND actor.status='active' AND owner.status='active'
 AND (actor.id=owner.id OR EXISTS(
  SELECT 1 FROM team_resource_bindings b JOIN teams t ON t.team_id=b.team_id
  LEFT JOIN team_members m ON m.team_id=t.team_id AND m.account_id=actor.id AND m.active
  WHERE b.resource_kind=$2 AND b.resource_id=$4 AND b.owner_account=owner.id AND b.active
   AND t.deleted_at IS NULL AND NOT t.suspended AND team_role_allows(CASE WHEN t.owner_account=actor.id THEN 'owner' ELSE m.role END,'settings.manage')
 ))
)`, actor, kind, owner, id).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrNotAuthorized
	}
	return nil
}

func requireBoundPolicyTeam(ctx context.Context, tx *db.Tx, actor, kind, id, owner, team string) error {
	if err := teams.Lock(ctx, tx); err != nil {
		return err
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM teams t JOIN users u ON u.id=$1 AND u.status='active'
 JOIN team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind=$2 AND b.resource_id=$3 AND b.owner_account=$4 AND b.active
 LEFT JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$1 AND m.active
 WHERE t.team_id=$5 AND t.deleted_at IS NULL AND NOT t.suspended
  AND (t.owner_account=$1 OR m.account_id=$1)
  AND ($1=$4 OR team_role_allows(CASE WHEN t.owner_account=$1 THEN 'owner' ELSE m.role END,'settings.manage'))
)`, actor, kind, id, owner, team).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotAuthorized
	}
	return nil
}

func ensurePolicyEdgeMode(ctx context.Context, tx *db.Tx, kind, route, nativeMode, audience string) error {
	if kind == "preview" {
		if nativeMode == "public" {
			if audience != AudiencePublic {
				return ErrPolicyEdgeTransition
			}
			return nil
		}
		if audience != AudienceOwner {
			return ErrPolicyWidening
		}
		return nil
	}
	if kind != "tunnel" {
		return ErrInvalid
	}
	if nativeMode == "public" {
		// Only public HTTP tunnel routes can be converted into an edge restricted
		// serving mode. The route assignment worker carries the exact policy
		// generation and waits for its fenced replacement to become ready.
		return nil
	}
	if audience != AudienceOwner {
		// No ACL-intersection exists yet for native private/team routes. Until it
		// does, do not let browser-only selectors add another principal.
		return ErrPolicyWidening
	}
	return nil
}

func readViewerPolicy(ctx context.Context, tx *db.Tx, out *ViewerPolicy) error {
	err := tx.QueryRow(ctx, `SELECT audience,COALESCE(team_id,''),allowed_emails,COALESCE(email_domain,''),generation,updated_at FROM browser_viewer_policies WHERE resource_kind=$1 AND resource_id=$2 AND route_id=$3 AND owner_account_id=$4`, out.ResourceKind, out.ResourceID, out.RouteID, out.OwnerAccountID).Scan(&out.Audience, &out.TeamID, &out.AllowedEmails, &out.EmailDomain, &out.Generation, &out.UpdatedAt)
	if err == pgx.ErrNoRows {
		out.Configured = false
		out.Generation = 0
		out.Audience = ""
		out.TeamID = ""
		out.AllowedEmails = nil
		out.EmailDomain = ""
		return nil
	}
	if err != nil {
		return err
	}
	out.Configured = true
	return nil
}

func applyViewerPolicy(ctx context.Context, tx *db.Tx, a authority, account string) (authority, error) {
	var p ViewerPolicy
	p.ResourceKind, p.ResourceID, p.RouteID = a.kind, a.id, a.route
	p.OwnerAccountID = a.owner
	err := readViewerPolicy(ctx, tx, &p)
	if err != nil {
		return a, err
	}
	if !p.Configured {
		if a.kind == "tunnel" || a.kind == "preview" {
			a.policyGeneration = a.generation
			if a.kind == "tunnel" {
				// Route/resource edits do not revoke an unchanged viewer policy.
				if err := tx.QueryRow(ctx, `SELECT viewer_policy_generation FROM current_tunnel_route_viewer_policy WHERE route_id=$1`, a.route).Scan(&a.policyGeneration); err != nil {
					return a, err
				}
			}
		}
		return applyPrincipal(ctx, tx, a, account)
	}
	if p.OwnerAccountID != a.owner {
		return a, ErrNotAuthorized
	}
	if a.kind == "tunnel" || a.kind == "preview" {
		if a.nativeMode == "" {
			a.nativeMode = a.mode
		}
		if a.kind == "tunnel" {
			if a.nativeMode != "public" && p.Audience != AudienceOwner {
				// Until the native ACL can be intersected with viewer selectors,
				// configured non-owner audiences may not widen a native private/team
				// route. Fail closed for legacy rows written before this guard existed.
				return a, ErrNotAuthorized
			}
			if p.Audience == AudiencePublic && a.nativeMode != "public" {
				return a, ErrNotAuthorized
			}
			// The audience is enforced here as a browser grant. The edge only needs
			// to know whether the route is public or requires an authenticated grant;
			// selected-team membership is not a native team grant.
			a.mode = effectiveTunnelViewerMode(p.Audience)
		} else if a.nativeMode == "public" {
			if p.Audience != AudiencePublic {
				return a, ErrNotAuthorized
			}
		} else if p.Audience != AudienceOwner {
			return a, ErrNotAuthorized
		}
	}
	var actorEmail string
	var emailVerified, actorActive, ownerActive bool
	if err = tx.QueryRow(ctx, `SELECT actor.primary_email,actor.email_verified,actor.status='active',owner.status='active' FROM users actor JOIN users owner ON owner.id=$2 WHERE actor.id=$1`, account, a.owner).Scan(&actorEmail, &emailVerified, &actorActive, &ownerActive); err != nil {
		return a, mapMissing(err)
	}
	if !actorActive || !ownerActive {
		return a, ErrNotAuthorized
	}
	if account != a.owner {
		switch p.Audience {
		case AudiencePublic:
		case AudienceOwner:
			return a, ErrNotAuthorized
		case AudienceTeam:
			if err = teams.Lock(ctx, tx); err != nil {
				return a, err
			}
			if err = tx.QueryRow(ctx, `SELECT t.generation,m.membership_generation,b.generation
FROM teams t
JOIN team_members m ON m.team_id=t.team_id AND m.account_id=$2 AND m.active
JOIN team_resource_bindings b ON b.team_id=t.team_id
 AND b.resource_kind=$3 AND b.resource_id=$4 AND b.owner_account=$5 AND b.active
WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended`, p.TeamID, account, a.kind, a.id, a.owner).
				Scan(&a.policyTeamGeneration, &a.policyMembershipGeneration, &a.policyBindingGeneration); err != nil {
				if err == pgx.ErrNoRows {
					return a, ErrNotAuthorized
				}
				return a, err
			}
			a.policyTeam = p.TeamID
		case AudienceSpecificEmails:
			if !emailVerified {
				return a, ErrNotAuthorized
			}
			canonical, ok := canonicalEmail(actorEmail)
			if !ok || !containsEmail(p.AllowedEmails, canonical) {
				return a, ErrNotAuthorized
			}
		case AudienceEmailDomain:
			if !emailVerified {
				return a, ErrNotAuthorized
			}
			_, domain, found := strings.Cut(actorEmail, "@")
			canonical, ok := canonicalDomain(domain)
			if !found || !ok || canonical != p.EmailDomain {
				return a, ErrNotAuthorized
			}
		default:
			return a, ErrNotAuthorized
		}
	}
	a.policyAudience = p.Audience
	a.policyGeneration = p.Generation
	return a, nil
}

func effectiveTunnelViewerMode(audience string) string {
	switch audience {
	case AudiencePublic:
		return "public"
	case AudienceOwner, AudienceTeam, AudienceSpecificEmails, AudienceEmailDomain:
		return "private"
	default:
		return ""
	}
}

func effectiveViewerPolicyGeneration(a authority) int64 {
	if a.policyGeneration > 0 {
		return a.policyGeneration
	}
	return a.generation
}

func sameViewerPolicyTeam(a authority, team *string, teamGeneration, membershipGeneration, bindingGeneration *int64) bool {
	if a.policyTeam == "" {
		return team == nil && teamGeneration == nil && membershipGeneration == nil && bindingGeneration == nil
	}
	return team != nil && *team == a.policyTeam &&
		teamGeneration != nil && *teamGeneration == a.policyTeamGeneration &&
		membershipGeneration != nil && *membershipGeneration == a.policyMembershipGeneration &&
		bindingGeneration != nil && *bindingGeneration == a.policyBindingGeneration
}

func containsEmail(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}
