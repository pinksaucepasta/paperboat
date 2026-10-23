// Package teamidentity is team SSO and directory sync (PLAN F19-02..06) through WorkOS: one WorkOS
// organization per team that sets it up, self-serve setup in the WorkOS Admin Portal, state read
// from the WorkOS Events API, Require SSO with break-glass exemptions, and member provisioning.
package teamidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var (
	ErrNotConfigured     = errors.New("SSO is not configured on this server")
	ErrDirectoryTooLarge = errors.New("the directory has more users than one team can sync")
	ErrForbidden         = errors.New("your team role cannot manage SSO and directory sync")
	ErrNotFound          = errors.New("team not found")
	ErrInvalid           = errors.New("invalid SSO or directory setting")
	ErrTooFewSeats       = errors.New("SSO and directory sync need more seats")
	ErrNotReady          = errors.New("connect an identity provider and verify a domain before requiring SSO")
	ErrExemptLimit       = errors.New("too many SSO-exempt members")
)

// ReviewRequired is returned when turning on Require SSO would lock members out; the caller
// confirms by sending Digest back.
type ReviewRequired struct {
	Members []ReviewMember `json:"members"`
	Digest  string         `json:"digest"`
}

type ReviewMember struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
}

func (r *ReviewRequired) Error() string {
	return "turning on Require SSO locks out members without a recent SSO sign-in; review and confirm"
}

// SeatChanger buys seats as the team owner when provisioning needs them (billing.ChangeSeats).
type SeatChanger interface {
	EnsureSeats(ctx context.Context, owner, team string, quantity int, key string) error
}

type Service struct {
	db        *db.DB
	workos    WorkOS
	cfg       config.Business
	returnURL string
	seats     SeatChanger
	// invitationLifetime (seconds) is the configured invitation lifetime; held directory
	// invitations get it and each reconcile renews it.
	invitationLifetime int64
}

func New(store *db.DB, workos WorkOS, cfg config.Business, invitationLifetimeSeconds int64, returnURL string, seats SeatChanger) *Service {
	return &Service{db: store, workos: workos, cfg: cfg, invitationLifetime: invitationLifetimeSeconds, returnURL: returnURL, seats: seats}
}

type Status struct {
	Eligible     bool      `json:"eligible"`
	MinSeats     int       `json:"min_seats"`
	Seats        int       `json:"seats"`
	Organization bool      `json:"organization"`
	SSO          SSO       `json:"sso"`
	Directory    DirStatus `json:"directory"`
	CanManage    bool      `json:"can_manage"`
}

type SSO struct {
	Connections    []Connection `json:"connections"`
	Domains        []Domain     `json:"domains"`
	Required       bool         `json:"required"`
	SessionSeconds int          `json:"session_seconds"`
	SessionMin     int          `json:"session_min_seconds"`
	SessionMax     int          `json:"session_max_seconds"`
	JIT            bool         `json:"jit"`
	Exempt         []string     `json:"exempt"`
	ExemptMax      int          `json:"exempt_max"`
}

type DirStatus struct {
	Directory  *Directory  `json:"directory"`
	Users      int         `json:"users"`
	Groups     []GroupRole `json:"groups"`
	MappedOnly bool        `json:"mapped_only"`
	SyncedAt   *time.Time  `json:"synced_at,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type GroupRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type teamRow struct {
	owner, organization string
	sessionSeconds      *int
	required, jit       bool
	mappedOnly          bool
	seats               int
}

// authorizeTx returns the team and whether account may manage it (security.manage); auditors
// may read the status. It holds the team lock for the rest of the transaction.
func authorizeTx(ctx context.Context, tx *db.Tx, account, team string, manage bool) (teamRow, bool, error) {
	var t teamRow
	if err := teams.Lock(ctx, tx); err != nil {
		return t, false, err
	}
	var role *string
	err := tx.QueryRow(ctx, `SELECT t.owner_account,coalesce(t.workos_organization_id,''),t.sso_session_seconds,t.sso_required,t.sso_jit,t.directory_mapped_only,
 coalesce((SELECT quantity FROM billing_workspaces w WHERE w.team_id=t.team_id AND w.user_id IS NULL),0),
 (SELECT CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active)
 FROM teams t WHERE t.team_id=$1 AND t.deleted_at IS NULL`, team, account).Scan(&t.owner, &t.organization, &t.sessionSeconds, &t.required, &t.jit, &t.mappedOnly, &t.seats, &role)
	if err != nil || role == nil {
		return t, false, ErrNotFound
	}
	canManage := teams.Allowed(*role, teams.PermissionSecurityManage)
	if !canManage && (manage || !teams.Allowed(*role, teams.PermissionAuditRead)) {
		return t, false, ErrForbidden
	}
	return t, canManage, nil
}

// Status reads the team's SSO and directory state as last synced from WorkOS.
func (s *Service) Status(ctx context.Context, account, team string) (Status, error) {
	out := Status{MinSeats: s.cfg.SSOMinSeats}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		t, canManage, err := authorizeTx(ctx, tx, account, team, false)
		if err != nil {
			return err
		}
		out.CanManage, out.Seats, out.Organization = canManage, t.seats, t.organization != ""
		out.Eligible = t.seats >= s.cfg.SSOMinSeats
		out.SSO = SSO{Required: t.required, JIT: t.jit, SessionSeconds: int(s.cfg.SSOSessionDefault.Seconds()),
			SessionMin: int(s.cfg.SSOSessionMin.Seconds()), SessionMax: int(s.cfg.SSOSessionMax.Seconds()), ExemptMax: s.cfg.SSOExemptMax,
			Connections: []Connection{}, Domains: []Domain{}, Exempt: []string{}}
		if t.sessionSeconds != nil {
			out.SSO.SessionSeconds = *t.sessionSeconds
		}
		out.Directory = DirStatus{MappedOnly: t.mappedOnly, Groups: []GroupRole{}}
		if err = readExempt(ctx, tx, team, &out.SSO.Exempt); err != nil {
			return err
		}
		return readState(ctx, tx, team, &out)
	})
	return out, err
}

func readExempt(ctx context.Context, tx *db.Tx, team string, out *[]string) error {
	rows, err := tx.Query(ctx, `SELECT account_id FROM team_members WHERE team_id=$1 AND active AND sso_exempt ORDER BY account_id`, team)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		*out = append(*out, id)
	}
	return rows.Err()
}

func readState(ctx context.Context, tx *db.Tx, team string, out *Status) error {
	var domains, connections, groups []byte
	var directory []byte
	var synced *time.Time
	var syncError *string
	err := tx.QueryRow(ctx, `SELECT domains,connections,directory,directory_groups,directory_users,synced_at,sync_error FROM team_identity_state WHERE team_id=$1`, team).
		Scan(&domains, &connections, &directory, &groups, &out.Directory.Users, &synced, &syncError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // a team that never set up SSO has no state yet
	}
	if err != nil {
		return err
	}
	_ = json.Unmarshal(domains, &out.SSO.Domains)
	_ = json.Unmarshal(connections, &out.SSO.Connections)
	if len(directory) > 0 {
		_ = json.Unmarshal(directory, &out.Directory.Directory)
	}
	var all []DirectoryGroup
	_ = json.Unmarshal(groups, &all)
	roles := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT group_id,role FROM team_directory_group_roles WHERE team_id=$1`, team)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			rows.Close()
			return err
		}
		roles[id] = role
	}
	rows.Close()
	for _, g := range all {
		out.Directory.Groups = append(out.Directory.Groups, GroupRole{ID: g.ID, Name: g.Name, Role: roles[g.ID]})
	}
	out.Directory.SyncedAt = synced
	if syncError != nil {
		out.Directory.Error = *syncError
	}
	return rows.Err()
}

// Intents the Admin Portal supports for team setup.
var portalIntents = map[string]bool{"domain_verification": true, "sso": true, "dsync": true}

// PortalLink creates the team's WorkOS organization on first use and returns a short-lived Admin
// Portal link for one setup step. The team's IT admin finishes setup there.
func (s *Service) PortalLink(ctx context.Context, account, team, intent string) (string, error) {
	if s.workos == nil {
		return "", ErrNotConfigured
	}
	if !portalIntents[intent] {
		return "", ErrInvalid
	}
	var t teamRow
	var name string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		if t, _, err = authorizeTx(ctx, tx, account, team, true); err != nil {
			return err
		}
		if t.seats < s.cfg.SSOMinSeats {
			return ErrTooFewSeats
		}
		return tx.QueryRow(ctx, `SELECT coalesce(display_name,team_id) FROM teams WHERE team_id=$1`, team).Scan(&name)
	})
	if err != nil {
		return "", err
	}
	if t.organization == "" {
		// The idempotency key makes a retried creation return the same organization.
		org, err := s.workos.CreateOrganization(ctx, name, team, "paperboat-team-org-"+team)
		if err != nil {
			return "", err
		}
		if _, err = s.db.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET workos_organization_id=coalesce(workos_organization_id,$2) WHERE team_id=$1`, team, org); err != nil {
			return "", err
		}
		if _, err = s.db.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_identity_state(team_id) VALUES($1) ON CONFLICT DO NOTHING`, team); err != nil {
			return "", err
		}
		if err = s.db.SQL().QueryRowContext(ctx, `SELECT workos_organization_id FROM paperboat.teams WHERE team_id=$1`, team).Scan(&t.organization); err != nil {
			return "", err
		}
	}
	link, err := s.workos.PortalLink(ctx, t.organization, intent, s.returnURL+"?team="+url.QueryEscape(team)+"&tab=security")
	if err != nil {
		return "", err
	}
	return link, s.record(ctx, account, team, "sso_portal_opened", map[string]any{"intent": intent})
}

// SSOSettings changes Require SSO, the SSO session length and JIT provisioning. Turning Require
// SSO on returns *ReviewRequired unless Confirm is the digest of the members who would lose access.
type SSOSettings struct {
	Required       *bool  `json:"required,omitempty"`
	SessionSeconds *int   `json:"session_seconds,omitempty"`
	JIT            *bool  `json:"jit,omitempty"`
	Confirm        string `json:"confirm,omitempty"`
}

func (s *Service) UpdateSSO(ctx context.Context, account, team string, in SSOSettings) (Status, error) {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		t, _, err := authorizeTx(ctx, tx, account, team, true)
		if err != nil {
			return err
		}
		session := int(s.cfg.SSOSessionDefault.Seconds())
		if t.sessionSeconds != nil {
			session = *t.sessionSeconds
		}
		if in.SessionSeconds != nil {
			if *in.SessionSeconds < int(s.cfg.SSOSessionMin.Seconds()) || *in.SessionSeconds > int(s.cfg.SSOSessionMax.Seconds()) {
				return ErrInvalid
			}
			session = *in.SessionSeconds
		}
		required, jit := t.required, t.jit
		if in.JIT != nil {
			jit = *in.JIT
		}
		if in.Required != nil {
			required = *in.Required
		}
		if required && !t.required {
			if err = s.readyForRequiredTx(ctx, tx, team, t); err != nil {
				return err
			}
			if err = reviewTx(ctx, tx, team, session, in.Confirm); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE teams SET sso_required=$2,sso_session_seconds=$3,sso_jit=$4,generation=generation+1 WHERE team_id=$1`, team, required, session, jit); err != nil {
			return err
		}
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "sso_settings", map[string]any{"required": required, "session_seconds": session, "jit": jit})
	})
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, account, team)
}

func (s *Service) readyForRequiredTx(ctx context.Context, tx *db.Tx, team string, t teamRow) error {
	if t.seats < s.cfg.SSOMinSeats {
		return ErrTooFewSeats
	}
	var ready bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_sso_domains WHERE team_id=$1) AND EXISTS(SELECT 1 FROM team_identity_state s, jsonb_array_elements(s.connections) c WHERE s.team_id=$1 AND c->>'state'='active')`, team).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return ErrNotReady
	}
	return nil
}

// reviewTx lists the members who would lose access (not the owner, not exempt, no SSO sign-in
// within the session length) and refuses unless confirm is the digest of exactly that list.
func reviewTx(ctx context.Context, tx *db.Tx, team string, session int, confirm string) error {
	rows, err := tx.Query(ctx, `SELECT m.account_id,u.primary_email FROM team_members m JOIN teams t USING(team_id) JOIN users u ON u.id=m.account_id
 WHERE m.team_id=$1 AND m.active AND m.account_id<>t.owner_account AND NOT m.sso_exempt
  AND (m.sso_verified_at IS NULL OR m.sso_verified_at <= now()-make_interval(secs=>$2)) ORDER BY m.account_id`, team, session)
	if err != nil {
		return err
	}
	review := &ReviewRequired{Members: []ReviewMember{}}
	for rows.Next() {
		var m ReviewMember
		if err := rows.Scan(&m.AccountID, &m.Email); err != nil {
			rows.Close()
			return err
		}
		review.Members = append(review.Members, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	ids := make([]string, len(review.Members))
	for i, m := range review.Members {
		ids[i] = m.AccountID
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(team + "\n" + strings.Join(ids, "\n")))
	review.Digest = hex.EncodeToString(sum[:])
	if len(review.Members) > 0 && confirm != review.Digest {
		return review
	}
	return nil
}

// SetExempt marks a member SSO exempt (break-glass) or removes the mark.
func (s *Service) SetExempt(ctx context.Context, account, team, member string, exempt bool) (Status, error) {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, _, err := authorizeTx(ctx, tx, account, team, true); err != nil {
			return err
		}
		if exempt {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1 AND active AND sso_exempt AND account_id<>$2`, team, member).Scan(&count); err != nil {
				return err
			}
			if count >= s.cfg.SSOExemptMax {
				return ErrExemptLimit
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE team_members SET sso_exempt=$3 WHERE team_id=$1 AND account_id=$2 AND active`, team, member, exempt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrInvalid
		}
		if _, err = tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=$1`, team); err != nil {
			return err
		}
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "sso_exempt", map[string]any{"target_account": member, "exempt": exempt})
	})
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, account, team)
}

// DirectorySettings maps directory groups to roles ("" removes a mapping) and sets whether only
// members of mapped groups are provisioned.
type DirectorySettings struct {
	MappedOnly *bool             `json:"mapped_only,omitempty"`
	GroupRoles map[string]string `json:"group_roles,omitempty"`
}

func (s *Service) UpdateDirectory(ctx context.Context, account, team string, in DirectorySettings) (Status, error) {
	if len(in.GroupRoles) > 500 {
		return Status{}, ErrInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		t, _, err := authorizeTx(ctx, tx, account, team, true)
		if err != nil {
			return err
		}
		mappedOnly := t.mappedOnly
		if in.MappedOnly != nil {
			mappedOnly = *in.MappedOnly
		}
		for group, role := range in.GroupRoles {
			if len(group) == 0 || len(group) > 128 {
				return ErrInvalid
			}
			if role == "" {
				if _, err = tx.Exec(ctx, `DELETE FROM team_directory_group_roles WHERE team_id=$1 AND group_id=$2`, team, group); err != nil {
					return err
				}
				continue
			}
			if !teams.AssignableRole(role) {
				return ErrInvalid
			}
			if _, err = tx.Exec(ctx, `INSERT INTO team_directory_group_roles(team_id,group_id,role) VALUES($1,$2,$3) ON CONFLICT(team_id,group_id) DO UPDATE SET role=EXCLUDED.role`, team, group, role); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE teams SET directory_mapped_only=$2 WHERE team_id=$1`, team, mappedOnly); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE team_identity_state SET dirty=true WHERE team_id=$1`, team); err != nil {
			return err
		}
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "directory_settings", map[string]any{"mapped_only": mappedOnly})
	})
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, account, team)
}

func (s *Service) record(ctx context.Context, account, team, action string, metadata map[string]any) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, action, metadata)
	})
}
