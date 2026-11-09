// Package workspacedomain owns the domains a workspace adds once and then uses for any tunnel or preview
// hostname under them (ngrok reserved domains, Vercel team domains). A domain is verified once by its
// _acme-challenge CNAME, which also delegates DNS-01 for one wildcard certificate (the domain and *.domain)
// that every hostname under it reuses. Tunnel and preview hostnames point at the workspace domain.
package workspacedomain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/publicationdns"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

var (
	ErrInvalidHostname = errors.New("domain is invalid")
	ErrReserved        = errors.New("domain belongs to Paperboat")
	ErrConflict        = errors.New("domain is verified in another workspace")
	ErrNotFound        = errors.New("domain not found")
	ErrForbidden       = errors.New("only team owners and admins add or remove team domains")
	ErrInUse           = errors.New("domain is used by tunnels or previews")
	ErrNotCovered      = errors.New("hostname is not under a domain of this workspace")
	ErrNotVerified     = errors.New("domain is not verified yet")
	ErrUnavailable     = errors.New("workspace domains are not configured on this server")
)

// Config is the server configuration the DNS instructions come from.
type Config struct {
	ChallengeZone string // Paperboat-owned zone the _acme-challenge CNAMEs point into
	RoutingBase   string // the platform tunnel wildcard base the routing CNAME points into
}

type Service struct {
	db  *db.DB
	cfg Config
	now func() time.Time
}

func New(database *db.DB, cfg Config) *Service {
	return &Service{db: database, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

// Record is one DNS record the user adds at their provider.
type Record struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Purpose  string `json:"purpose"` // verification, routing or apex_routing
	Required bool   `json:"required"`
}

// Use is a tunnel or preview hostname under the domain.
type Use struct {
	Kind             string `json:"kind"` // tunnel or preview
	ID               string `json:"id"`
	Name             string `json:"name"` // tunnel name or preview address
	Hostname         string `json:"hostname"`
	DomainBindingID  string `json:"domain_binding_id"`
	CertificateState string `json:"certificate_state"`
}

type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type View struct {
	ID          string   `json:"id"`
	Hostname    string   `json:"hostname"`
	Workspace   string   `json:"workspace"`
	State       string   `json:"state"` // pending_dns, verified, failed
	Records     []Record `json:"records"`
	Observed    []string `json:"observed_records"`
	CheckedAt   *string  `json:"last_checked_at,omitempty"`
	NextCheckAt string   `json:"next_check_at"`
	VerifiedAt  *string  `json:"verified_at,omitempty"`
	DNSFailure  *Failure `json:"dns_failure,omitempty"`
	Certificate struct {
		State     string   `json:"state"` // pending, ready, failed
		ExpiresAt *string  `json:"expires_at,omitempty"`
		Failure   *Failure `json:"failure,omitempty"`
	} `json:"certificate"`
	UsedBy    []Use  `json:"used_by"`
	CreatedAt string `json:"created_at"`
}

func scopeOf(ctx context.Context) workspace.Scope {
	if ws, ok := workspace.FromContext(ctx); ok {
		return ws
	}
	return workspace.Scope{ID: "personal"}
}

func teamArg(ws workspace.Scope) sql.NullString {
	return sql.NullString{String: ws.TeamID, Valid: ws.TeamID != ""}
}

// scopeKey names the workspace in the challenge target: the same workspace always gets the same target.
func scopeKey(account string, team sql.NullString) string {
	if team.Valid {
		return "team:" + team.String
	}
	return "account:" + account
}

var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHostname accepts an apex or a subdomain the user controls: no wildcard, no IP, no public suffix.
func NormalizeHostname(raw string) (string, error) {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "https://"), ".")
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 || net.ParseIP(ascii) != nil {
		return "", ErrInvalidHostname
	}
	labels := strings.Split(ascii, ".")
	for _, label := range labels {
		if !labelPattern.MatchString(label) {
			return "", ErrInvalidHostname
		}
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(ascii); err != nil || len(labels) < 2 {
		return "", ErrInvalidHostname
	}
	return ascii, nil
}

// requireManager allows a personal workspace's owner, or a team's owners and admins.
func requireManager(ctx context.Context, tx *db.Tx, account string, ws workspace.Scope) error {
	if ws.TeamID == "" {
		return nil
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.teams t LEFT JOIN paperboat.team_members m ON m.team_id=t.team_id AND m.account_id=$2 AND m.active
 WHERE t.team_id=$1 AND t.deleted_at IS NULL AND (t.owner_account=$2 OR (m.account_id IS NOT NULL AND paperboat.team_role_allows(m.role,'settings.manage'))))`, ws.TeamID, account).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

// Add claims hostname for the selected workspace. A hostname another workspace added but never verified is
// taken over: only a verified domain blocks a claim, so nobody can squat a domain they do not control.
func (s *Service) Add(ctx context.Context, account, actor, raw string) (View, error) {
	if s.cfg.ChallengeZone == "" || s.cfg.RoutingBase == "" {
		return View{}, ErrUnavailable
	}
	hostname, err := NormalizeHostname(raw)
	if err != nil {
		return View{}, err
	}
	ws := scopeOf(ctx)
	if err := workspace.Validate(ctx, s.db, account, ws); err != nil {
		return View{}, err
	}
	id, err := newID()
	if err != nil {
		return View{}, err
	}
	var row dbsqlc.WorkspaceDomain
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireManager(ctx, tx, account, ws); err != nil {
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT paperboat.selfhost_publication_hostname_allowed($1)`, hostname).Scan(&allowed); err != nil {
			return err
		}
		if !allowed || hostname == s.cfg.ChallengeZone || strings.HasSuffix(hostname, "."+s.cfg.ChallengeZone) ||
			hostname == s.cfg.RoutingBase || strings.HasSuffix(hostname, "."+s.cfg.RoutingBase) {
			return ErrReserved
		}
		q := tx.Queries()
		team := teamArg(ws)
		existing, err := q.GetWorkspaceDomainByHostname(ctx, hostname)
		switch {
		case err == nil:
			sameWorkspace := existing.OwnerTeamID == team && (team.Valid || existing.AccountID == account)
			if sameWorkspace {
				row = existing
				return nil
			}
			if existing.OwnershipState == "verified" {
				return ErrConflict
			}
			if _, err := q.DeleteWorkspaceDomain(ctx, existing.ID); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		row, err = q.CreateWorkspaceDomain(ctx, dbsqlc.CreateWorkspaceDomainParams{ID: id, AccountID: account, OwnerTeamID: team,
			Hostname: hostname, ChallengeReference: scopeKey(account, team), CreatedByActorID: actor, Now: s.now()})
		return err
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, row)
}

func (s *Service) List(ctx context.Context, account string) ([]View, error) {
	ws := scopeOf(ctx)
	if err := workspace.Validate(ctx, s.db, account, ws); err != nil {
		return nil, err
	}
	rows, err := s.db.Queries().ListWorkspaceDomains(ctx, dbsqlc.ListWorkspaceDomainsParams{AccountID: account, TeamID: teamArg(ws)})
	if err != nil {
		return nil, err
	}
	return s.views(ctx, rows)
}

func (s *Service) get(ctx context.Context, account, id string) (dbsqlc.WorkspaceDomain, workspace.Scope, error) {
	ws := scopeOf(ctx)
	if err := workspace.Validate(ctx, s.db, account, ws); err != nil {
		return dbsqlc.WorkspaceDomain{}, ws, err
	}
	row, err := s.db.Queries().GetWorkspaceDomain(ctx, dbsqlc.GetWorkspaceDomainParams{ID: id, AccountID: account, TeamID: teamArg(ws)})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ws, ErrNotFound
	}
	return row, ws, err
}

func (s *Service) Get(ctx context.Context, account, id string) (View, error) {
	row, _, err := s.get(ctx, account, id)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, row)
}

// Check asks for a DNS check now (the console's "Check now", `pb domain verify`); the worker runs it.
func (s *Service) Check(ctx context.Context, account, id string) (View, error) {
	row, _, err := s.get(ctx, account, id)
	if err != nil {
		return View{}, err
	}
	if row, err = s.db.Queries().CheckWorkspaceDomainNow(ctx, dbsqlc.CheckWorkspaceDomainNowParams{ID: row.ID, Now: s.now()}); err != nil {
		return View{}, err
	}
	return s.view(ctx, row)
}

// InUseError lists where a domain is used, so removal can say what to change first.
type InUseError struct{ Uses []Use }

func (e *InUseError) Error() string { return ErrInUse.Error() }
func (e *InUseError) Unwrap() error { return ErrInUse }

// Remove deletes a domain no tunnel or preview uses. Its certificate copies live with those hostnames, so a
// domain in use is refused with the list of uses instead of breaking them.
func (s *Service) Remove(ctx context.Context, account, id string) error {
	row, ws, err := s.get(ctx, account, id)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireManager(ctx, tx, account, ws); err != nil {
			return err
		}
		// Lock the row so a hostname bound concurrently either commits first (and is listed) or waits.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM paperboat.workspace_domains WHERE id=$1 FOR UPDATE`, row.ID); err != nil {
			return err
		}
		uses, err := listUses(ctx, tx.Queries(), []string{row.ID})
		if err != nil {
			return err
		}
		if len(uses[row.ID]) > 0 {
			return &InUseError{Uses: uses[row.ID]}
		}
		n, err := tx.Queries().DeleteWorkspaceDomain(ctx, row.ID)
		if err == nil && n == 0 {
			return ErrNotFound
		}
		return err
	})
}

// Covering is the verified workspace domain a tunnel or preview hostname lives under, looked up inside the
// caller's transaction (FOR SHARE, so the domain cannot be removed before the hostname commits). The hostname
// is the domain itself, one label under it, or the one-label wildcard *.domain: what its certificate covers.
func Covering(ctx context.Context, tx *db.Tx, account string, team sql.NullString, hostname string) (dbsqlc.WorkspaceDomain, error) {
	base := strings.TrimPrefix(hostname, "*.")
	parent := base
	if !strings.HasPrefix(hostname, "*.") {
		if i := strings.IndexByte(base, '.'); i > 0 {
			parent = base[i+1:]
		}
	}
	row, err := tx.Queries().GetCoveringWorkspaceDomain(ctx, dbsqlc.GetCoveringWorkspaceDomainParams{Hostname: base, Parent: parent, AccountID: account, TeamID: team})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrNotCovered
	}
	if err != nil {
		return row, err
	}
	if strings.HasPrefix(hostname, "*.") && row.Hostname != base {
		return row, ErrNotCovered
	}
	if row.OwnershipState != "verified" {
		return row, ErrNotVerified
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM paperboat.workspace_domains WHERE id=$1 FOR SHARE`, row.ID); err != nil {
		return row, err
	}
	return row, nil
}

func listUses(ctx context.Context, q *dbsqlc.Queries, ids []string) (map[string][]Use, error) {
	rows, err := q.ListWorkspaceDomainUses(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string][]Use{}
	for _, r := range rows {
		out[r.WorkspaceDomainID] = append(out[r.WorkspaceDomainID], Use{Kind: r.Kind, ID: r.ResourceID, Name: r.ResourceName,
			Hostname: r.Hostname, DomainBindingID: r.DomainID, CertificateState: r.CertificateState})
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, row dbsqlc.WorkspaceDomain) (View, error) {
	views, err := s.views(ctx, []dbsqlc.WorkspaceDomain{row})
	if err != nil {
		return View{}, err
	}
	return views[0], nil
}

func (s *Service) views(ctx context.Context, rows []dbsqlc.WorkspaceDomain) ([]View, error) {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	uses, err := listUses(ctx, s.db.Queries(), ids)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		records, err := s.Records(ctx, r)
		if err != nil {
			return nil, err
		}
		v := viewOf(r, records)
		if u := uses[r.ID]; u != nil {
			v.UsedBy = u
		}
		out = append(out, v)
	}
	return out, nil
}

// Records are the DNS records to add: the verification CNAME (required) and the routing records to the
// workspace's edges, for every name under the domain and, optionally, for the domain itself.
func (s *Service) Records(ctx context.Context, row dbsqlc.WorkspaceDomain) ([]Record, error) {
	challenge, err := tunnelcert.WorkspaceChallengeTarget(row.ChallengeReference, row.Hostname, s.cfg.ChallengeZone)
	if err != nil {
		return nil, err
	}
	records := []Record{{Name: "_acme-challenge." + row.Hostname, Type: "CNAME", Value: challenge, Purpose: "verification", Required: true}}
	targets, err := s.Targets(ctx, row)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		records = append(records, Record{Name: "*." + row.Hostname, Type: publicationdns.DomainRecordType(target, "*."+row.Hostname, "generic"), Value: target, Purpose: "routing", Required: true})
	}
	for _, target := range targets {
		records = append(records, Record{Name: row.Hostname, Type: publicationdns.DomainRecordType(target, row.Hostname, "generic"), Value: target, Purpose: "apex_routing"})
	}
	return records, nil
}

// Targets are where the domain's names point: a per-domain name under the platform tunnel wildcard for
// managed edges, or the addresses of the workspace's self-hosted edges.
func (s *Service) Targets(ctx context.Context, row dbsqlc.WorkspaceDomain) ([]string, error) {
	var targets []string
	err := s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		targets, err = publicationdns.WorkspaceTargetsTx(ctx, tx, row.AccountID, row.OwnerTeamID, RoutingName(row.ID, s.cfg.RoutingBase))
		return err
	})
	if errors.Is(err, publicationdns.ErrSelfhostPlacementRequired) {
		return nil, nil // shown without routing records until a self-hosted edge has verified public ingress
	}
	return targets, err
}

// RoutingName is the managed routing target of a workspace domain: any name under the platform wildcard
// reaches the edges, and a per-domain name lets routing move without the user changing DNS.
func RoutingName(id, base string) string {
	return "d-" + strings.TrimPrefix(id, "wsd_")[:12] + "." + base
}

func viewOf(r dbsqlc.WorkspaceDomain, records []Record) View {
	v := View{ID: r.ID, Hostname: r.Hostname, Workspace: "personal", Records: records, UsedBy: []Use{}, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339)}
	if r.OwnerTeamID.Valid {
		v.Workspace = r.OwnerTeamID.String
	}
	switch r.OwnershipState {
	case "verified":
		v.State = "verified"
	case "failed":
		v.State = "failed"
	default:
		v.State = "pending_dns"
	}
	if json.Unmarshal(r.ObservedRecords, &v.Observed) != nil || v.Observed == nil {
		v.Observed = []string{}
	}
	stamp := func(t sql.NullTime) *string {
		if !t.Valid {
			return nil
		}
		value := t.Time.UTC().Format(time.RFC3339)
		return &value
	}
	v.CheckedAt, v.VerifiedAt = stamp(r.DnsLastCheckedAt), stamp(r.VerifiedAt)
	v.NextCheckAt = r.DnsNextCheckAt.UTC().Format(time.RFC3339)
	if r.DnsFailureCode.Valid && r.OwnershipState != "verified" {
		v.DNSFailure = &Failure{Code: r.DnsFailureCode.String, Message: dnsFailureMessage(r.DnsFailureCode.String, r.Hostname)}
	}
	v.Certificate.State = r.CertificateState
	v.Certificate.ExpiresAt = stamp(r.CertificateExpiresAt)
	if r.CertificateFailureCode.Valid {
		v.Certificate.Failure = &Failure{Code: r.CertificateFailureCode.String, Message: certificateFailureMessage(r.CertificateFailureCode.String)}
	}
	return v
}

func dnsFailureMessage(code, hostname string) string {
	switch code {
	case "dns_not_found":
		return fmt.Sprintf("No CNAME at _acme-challenge.%s yet. Add the verification record; new records can take a few minutes to appear.", hostname)
	case "wrong_record":
		return fmt.Sprintf("_acme-challenge.%s points somewhere else. Change it to the verification value shown.", hostname)
	default:
		return "DNS could not be checked. Paperboat checks again automatically."
	}
}

func certificateFailureMessage(code string) string {
	switch code {
	case "caa_blocked":
		return "A CAA record on the domain does not allow Let's Encrypt. Add letsencrypt.org to it or remove it."
	case "issuer_rate_limited":
		return "Let's Encrypt is rate limiting this domain. Paperboat retries automatically."
	case "dns_challenge_pending", "dns_challenge_unavailable":
		return "The certificate check could not read the verification record. Keep the _acme-challenge CNAME in place; Paperboat retries."
	default:
		return "The certificate could not be issued yet. Paperboat retries automatically."
	}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "wsd_" + hex.EncodeToString(b[:]), nil
}
