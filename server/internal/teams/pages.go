package teams

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// DefaultPageLimit and MaximumPageLimit bound the member, grant and machine pages of a team.
const (
	DefaultPageLimit = 50
	MaximumPageLimit = 200
)

// Page is one keyset page; Total is every row the cursor pages through.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Total      int    `json:"total"`
}

// pageCursor is the position after the last row sent: the sort key's parts, bound to the team and the list so
// a cursor of one list cannot page another.
func encodePageCursor(team, list string, key ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(append([]string{team, list}, key...), "\x00")))
}

func decodePageCursor(team, list, cursor string, parts int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(cursor) > 1024 {
		return nil, ErrInvalid
	}
	fields := strings.Split(string(raw), "\x00")
	if len(fields) != parts+2 || fields[0] != team || fields[1] != list {
		return nil, ErrInvalid
	}
	return fields[2:], nil
}

func pageLimit(limit int) (int, error) {
	if limit < 0 || limit > MaximumPageLimit {
		return 0, ErrInvalid
	}
	if limit == 0 {
		return DefaultPageLimit, nil
	}
	return limit, nil
}

// readPage runs fn in one read-only snapshot for a current member of the team, then applies the SSO check Get does.
func (s *Service) readPage(ctx context.Context, account, team string, fn func(context.Context, *db.Tx) error) error {
	if !validID(team) {
		return ErrInvalid
	}
	var sso bool
	err := s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		role, err := viewerRoleTx(ctx, tx, team, account)
		if err != nil {
			return err
		}
		var deleted bool
		if err = tx.QueryRow(ctx, `SELECT sso_required,deleted_at IS NOT NULL FROM teams WHERE team_id=$1`, team).Scan(&sso, &deleted); err != nil {
			return mapTeamReadNotFound(err)
		}
		if role == "" || deleted {
			return ErrForbidden
		}
		return fn(ctx, tx)
	})
	if err != nil || !sso {
		return err
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error { return SSOGuardTx(ctx, tx, team, account) })
}

// Members pages the team's member rows by account id.
func (s *Service) Members(ctx context.Context, account, team, cursor string, limit int) (Page[Member], error) {
	out := Page[Member]{Items: []Member{}}
	limit, err := pageLimit(limit)
	if err != nil {
		return out, err
	}
	key, err := decodePageCursor(team, "members", cursor, 1)
	if err != nil {
		return out, err
	}
	after := ""
	if key != nil {
		after = key[0]
	}
	err = s.readPage(ctx, account, team, func(ctx context.Context, tx *db.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1`, team).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT m.account_id,m.membership_generation,CASE WHEN m.account_id=t.owner_account THEN 'owner' ELSE m.role END,m.active,u.primary_email,u.display_name,m.joined_at,m.directory_user_id IS NOT NULL,m.sso_exempt
 FROM team_members m JOIN teams t USING(team_id) JOIN users u ON u.id=m.account_id WHERE m.team_id=$1 AND m.account_id>$2 ORDER BY m.account_id LIMIT $3`, team, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.AccountID, &m.MembershipGeneration, &m.Role, &m.Active, &m.Email, &m.DisplayName, &m.JoinedAt, &m.DirectoryManaged, &m.SSOExempt); err != nil {
				return err
			}
			out.Items = append(out.Items, m)
		}
		return rows.Err()
	})
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.NextCursor = encodePageCursor(team, "members", out.Items[limit-1].AccountID)
	}
	return out, err
}

// Machines pages the machines shared into the team by machine id.
func (s *Service) Machines(ctx context.Context, account, team, cursor string, limit int) (Page[MachineBinding], error) {
	out := Page[MachineBinding]{Items: []MachineBinding{}}
	limit, err := pageLimit(limit)
	if err != nil {
		return out, err
	}
	key, err := decodePageCursor(team, "machines", cursor, 1)
	if err != nil {
		return out, err
	}
	after := ""
	if key != nil {
		after = key[0]
	}
	err = s.readPage(ctx, account, team, func(ctx context.Context, tx *db.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_resource_bindings b JOIN live_user_machines m ON m.id=b.resource_id WHERE b.team_id=$1 AND b.resource_kind='machine' AND b.active`, team).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT m.id,CASE WHEN m.owner_team_id IS NULL THEN m.user_id ELSE '' END,coalesce(m.owner_team_id,''),m.alias,m.state,m.online,m.configured_capabilities,b.generation,b.active FROM team_resource_bindings b JOIN live_user_machines m ON m.id=b.resource_id WHERE b.team_id=$1 AND b.resource_kind='machine' AND b.active AND m.id>$2 ORDER BY m.id LIMIT $3`, team, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b MachineBinding
			if err := rows.Scan(&b.MachineID, &b.OwnerAccount, &b.OwnerTeamID, &b.Alias, &b.State, &b.Online, &b.ConfiguredCapabilities, &b.Generation, &b.Active); err != nil {
				return err
			}
			out.Items = append(out.Items, b)
		}
		return rows.Err()
	})
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.NextCursor = encodePageCursor(team, "machines", out.Items[limit-1].MachineID)
	}
	return out, err
}

// Grants pages the team's grants: the stored rows by (resource kind, resource id, account), then the policy's
// grants in their own order. The cursor is the last stored row's key, or "policy" and the count already sent.
func (s *Service) Grants(ctx context.Context, account, team, cursor string, limit int) (Page[Grant], error) {
	out := Page[Grant]{Items: []Grant{}}
	limit, err := pageLimit(limit)
	if err != nil {
		return out, err
	}
	key, err := decodePageCursor(team, "grants", cursor, 3)
	if err != nil {
		return out, err
	}
	if key == nil {
		key = []string{"", "", ""}
	}
	policyOffset := -1
	if key[0] == "\x01policy" {
		if policyOffset, err = parseOffset(key[1]); err != nil {
			return out, err
		}
	}
	err = s.readPage(ctx, account, team, func(ctx context.Context, tx *db.Tx) error {
		var stored int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_resource_grants WHERE team_id=$1 AND `+grantRowFilter, team).Scan(&stored); err != nil {
			return err
		}
		policy, err := teamPolicyTx(ctx, tx, team)
		if err != nil {
			return err
		}
		fromPolicy := policyGrants(policy, team)
		out.Total = stored + len(fromPolicy)
		if policyOffset < 0 {
			rows, err := tx.Query(ctx, `SELECT account_id,resource_kind,resource_id,permission,generation,active FROM team_resource_grants WHERE team_id=$1 AND `+grantRowFilter+` AND (resource_kind,resource_id,account_id)>($2,$3,$4) ORDER BY resource_kind,resource_id,account_id LIMIT $5`, team, key[0], key[1], key[2], limit+1)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var g Grant
				if err := rows.Scan(&g.AccountID, &g.ResourceKind, &g.ResourceID, &g.Permission, &g.Generation, &g.Active); err != nil {
					return err
				}
				out.Items = append(out.Items, g)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(out.Items) > limit {
				return nil // more stored rows follow; the caller trims
			}
			policyOffset = 0
		}
		room := limit - len(out.Items)
		if policyOffset > len(fromPolicy) {
			return ErrInvalid
		}
		rest := fromPolicy[policyOffset:]
		if len(rest) > room {
			rest = rest[:room]
		}
		out.Items = append(out.Items, rest...)
		if next := policyOffset + len(rest); next < len(fromPolicy) {
			out.NextCursor = encodePageCursor(team, "grants", "\x01policy", strconv.Itoa(next), "")
		}
		return nil
	})
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[limit-1]
		out.NextCursor = encodePageCursor(team, "grants", last.ResourceKind, last.ResourceID, last.AccountID)
	}
	return out, err
}

func parseOffset(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, ErrInvalid
	}
	return n, nil
}
