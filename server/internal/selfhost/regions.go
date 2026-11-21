package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/text/unicode/norm"
)

// A region is where users see the owner's servers: servers of one region back each other up, and a workspace's pair
// names regions. It has a display name ("Home lab", console only, renameable) and a slug ("home-lab", what nodes, pbh,
// pairs and pools carry, never changed). A region that exists only as the region string of an enrolled server (the
// default "selfhost", or one typed at a pbh prompt) is implicit: it has no row, and its name is its slug until renamed
// (a rename stores the row). Managed regions are Paperboat's and never appear here.

var (
	ErrRegionExists   = errors.New("a region with that name already exists")
	ErrRegionNotFound = errors.New("region not found")
	ErrRegionInUse    = errors.New("region is in use")
)

const (
	regionNameMax = 48
	// regionLimit bounds the explicit regions of one scope (the implicit ones are bounded by the 256 installations).
	regionLimit = 64
	// regionSlugMax bounds a derived slug before its collision suffix.
	regionSlugMax = 40
	// defaultRegion is the region pbh enrols with when none is named: reserved so that a derived slug never takes it.
	defaultRegion = "selfhost"
)

// regionError is a refusal that carries the sentence the user is shown, and the sentinel the HTTP layer maps.
type regionError struct {
	message string
	base    error
}

func (e *regionError) Error() string { return e.message }
func (e *regionError) Unwrap() error { return e.base }

func invalidRegion(format string, args ...any) error {
	return &regionError{message: fmt.Sprintf(format, args...), base: ErrInvalid}
}

// Region is one region of the owner's scope as the console lists it.
type Region struct {
	// ID is the slug; it never changes.
	ID string `json:"id"`
	// Name is the display name, or the slug while the region is implicit and unnamed.
	Name string `json:"name"`
	// Machines is the number of distinct machines of the scope enrolled in the region; a machine that runs both a
	// relay and an edge (two components, two nodes) counts once.
	Machines int `json:"machines"`
	// Ready is how many of those machines are ready now (a machine is ready when any of its components is).
	Ready int `json:"ready"`
}

// regionNodesSQL (args: scope kind, scope id) is the distinct machines of the scope (one installation key, however many
// components, each with its own node) per region with their readiness, over non-retired nodes; managed nodes never count.
const regionNodesSQL = `SELECT coalesce(n.region,'') AS region, i.installation_public_key AS machine, bool_or(` + installationReadySQL + `) AS ready, bool_or(i.claim_complete) AS complete
 FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
 WHERE i.scope_kind=$1 AND i.scope_id=$2 AND i.state='active' AND n.state<>'retired' AND coalesce(n.region,'')<>''
  AND NOT EXISTS(SELECT 1 FROM paperboat.managed_node_authority m WHERE m.node_id=n.id)
 GROUP BY n.region, i.installation_public_key`

const listRegionsSQL = `WITH nodes AS (` + regionNodesSQL + `), counts AS (
 SELECT region AS slug, count(*)::int AS machines, (count(*) FILTER (WHERE ready))::int AS ready FROM nodes WHERE complete GROUP BY region
), slugs AS (
 SELECT slug FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2 UNION SELECT slug FROM counts
)
SELECT s.slug, coalesce(r.display_name,s.slug), coalesce(c.machines,0), coalesce(c.ready,0)
 FROM slugs s LEFT JOIN paperboat.selfhost_regions r ON r.scope_kind=$1 AND r.scope_id=$2 AND r.slug=s.slug LEFT JOIN counts c ON c.slug=s.slug
 ORDER BY lower(coalesce(r.display_name,s.slug)), s.slug`

// lockScope serializes everything that decides which regions a scope has: enrollment's reservation, region create,
// rename and delete. The key is the one reserve takes.
func lockScope(ctx context.Context, tx *sql.Tx, scope Scope) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1))`, scope.Kind+":"+scope.ID)
	return err
}

// regionScope authorizes the scope for a region read or write. Regions belong to Personal or a Team, never the
// operator's global scope; a write needs the same authority as enrolling a server.
func (s *Service) regionScope(ctx context.Context, actor string, scope Scope, write bool) (Scope, error) {
	if !s.available() {
		return scope, ErrUnauthorized
	}
	scope, err := s.authorizeScope(ctx, actor, scope, write, false)
	if err != nil {
		return scope, err
	}
	if scope.Kind != "account" && scope.Kind != "team" {
		return scope, ErrInvalid
	}
	return scope, nil
}

// ListRegions reads the scope's regions: the stored ones and every region its enrolled servers name, by name.
func (s *Service) ListRegions(ctx context.Context, actor string, scope Scope) ([]Region, error) {
	scope, err := s.regionScope(ctx, actor, scope, false)
	if err != nil {
		return nil, err
	}
	return listRegions(ctx, s.database.SQL(), scope)
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listRegions(ctx context.Context, q queryer, scope Scope) (out []Region, returnErr error) {
	rows, err := q.QueryContext(ctx, listRegionsSQL, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = withDatabaseCleanupFailure("list_regions", returnErr, rows.Close()) }()
	out = []Region{}
	for rows.Next() {
		var r Region
		if err = rows.Scan(&r.ID, &r.Name, &r.Machines, &r.Ready); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// validRegionName trims name and checks it is 1 to 48 printable characters.
func validRegionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > regionNameMax {
		return "", invalidRegion("Name the region with 1 to %d characters.", regionNameMax)
	}
	for _, r := range name {
		if !unicode.IsGraphic(r) || unicode.In(r, unicode.Cf) {
			return "", invalidRegion("Use letters, digits, spaces and punctuation in the region name.")
		}
	}
	return name, nil
}

// regionSlugBase derives a slug from a display name: NFKD, combining marks dropped, lowercase, runs of anything but
// [a-z0-9] become one '-', trimmed of '-'; a result that is empty or does not start with a letter gets the prefix "r-"
// (so "123" is "r-123"); cut to 40 characters. The result always satisfies validRegion (for an empty result, "r").
func regionSlugBase(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(name) {
		switch {
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
			fallthrough
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case unicode.Is(unicode.Mn, r):
			// Combining marks are dropped, not separators: "Ünïcode" is "unicode"; "Lab—West" is "lab-west".
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
		slug = "r-" + slug
	}
	if len(slug) > regionSlugMax {
		slug = slug[:regionSlugMax]
	}
	return strings.TrimRight(slug, "-")
}

// uniqueRegionSlug is base, or base-2, base-3 ... the first not in taken.
func uniqueRegionSlug(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		if slug := base + "-" + strconv.Itoa(n); !taken[slug] {
			return slug
		}
	}
}

// regionName is a region as the display-name check sees it.
type regionName struct{ display, slug string }

// regionTaken is what a new slug and a new display name must not collide with, read under the scope lock.
type regionTaken struct {
	// slugs are the explicit regions, the servers' regions (claimed or pending), Paperboat's regions and the default.
	slugs map[string]bool
	// names maps lowercase display name to its region: explicit names, and the slug of every implicit region (an
	// implicit region's name is its slug), including the default while nothing has named it.
	names map[string]regionName
}

func readRegionTaken(ctx context.Context, tx *sql.Tx, scope Scope) (regionTaken, error) {
	t := regionTaken{slugs: map[string]bool{defaultRegion: true}, names: map[string]regionName{}}
	rows, err := tx.QueryContext(ctx, `SELECT slug,display_name FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID)
	if err != nil {
		return t, err
	}
	explicit := map[string]bool{}
	for rows.Next() {
		var slug, name string
		if err = rows.Scan(&slug, &name); err != nil {
			return t, errors.Join(err, rows.Close())
		}
		explicit[slug] = true
		t.slugs[slug] = true
		t.names[strings.ToLower(name)] = regionName{name, slug}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return t, err
	}
	if !explicit[defaultRegion] {
		t.names[defaultRegion] = regionName{defaultRegion, defaultRegion}
	}
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT region FROM (`+regionNodesSQL+`) x`, scope.Kind, scope.ID)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var slug string
		if err = rows.Scan(&slug); err != nil {
			return t, errors.Join(err, rows.Close())
		}
		t.slugs[slug] = true
		if !explicit[slug] {
			t.names[slug] = regionName{slug, slug}
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return t, err
	}
	// Paperboat's own regions: a self-hosted region must not share an id with one a workspace's pair may also name.
	rows, err = tx.QueryContext(ctx, `SELECT id FROM paperboat.operator_regions UNION SELECT coalesce(n.region,'') FROM paperboat.managed_node_authority m JOIN paperboat.control_tunnel_nodes n ON n.id=m.node_id`)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var slug string
		if err = rows.Scan(&slug); err != nil {
			return t, errors.Join(err, rows.Close())
		}
		t.slugs[slug] = true
	}
	return t, errors.Join(rows.Err(), rows.Close())
}

// nameOwner is the region that already has this display name (case-insensitive), unless it is region skip.
func (t regionTaken) nameOwner(name, skip string) (string, bool) {
	owner, found := t.names[strings.ToLower(name)]
	if !found || owner.slug == skip {
		return "", false
	}
	return owner.display, true
}

func regionLimitError() error {
	return &regionError{message: fmt.Sprintf("A scope can have at most %d named regions; remove one first.", regionLimit), base: ErrLimit}
}

func regionExists(name string) error {
	return &regionError{message: fmt.Sprintf("A region called %q already exists.", name), base: ErrRegionExists}
}

// withRegionTx runs fn in a transaction that holds the scope's write authority and its region lock.
func (s *Service) withRegionTx(ctx context.Context, actor string, scope Scope, fn func(*sql.Tx) error) (returnErr error) {
	tx, err := s.database.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { returnErr = withTransactionRollbackFailure("region_write", returnErr, tx.Rollback()) }()
	if err = lockTeamWrite(ctx, tx, actor, scope); err != nil {
		return err
	}
	if err = lockScope(ctx, tx, scope); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateRegion stores a new region named name and returns it, with the slug derived from the name.
func (s *Service) CreateRegion(ctx context.Context, actor string, scope Scope, name string) (Region, error) {
	name, err := validRegionName(name)
	if err != nil {
		return Region{}, err
	}
	scope, err = s.regionScope(ctx, actor, scope, true)
	if err != nil {
		return Region{}, err
	}
	var out Region
	err = s.withRegionTx(ctx, actor, scope, func(tx *sql.Tx) error {
		taken, err := readRegionTaken(ctx, tx, scope)
		if err != nil {
			return err
		}
		if existing, found := taken.nameOwner(name, ""); found {
			return regionExists(existing)
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID).Scan(&count); err != nil {
			return err
		}
		if count >= regionLimit {
			return regionLimitError()
		}
		slug := uniqueRegionSlug(regionSlugBase(name), taken.slugs)
		if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES($1,$2,$3,$4)`, scope.Kind, scope.ID, slug, name); err != nil {
			return regionConflict(err, name)
		}
		out = Region{ID: slug, Name: name}
		return nil
	})
	return out, err
}

// regionConflict maps the unique index on the display name (a concurrent writer that bypassed the lock) to the
// same answer the lock-protected check gives.
func regionConflict(err error, name string) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return regionExists(name)
	}
	return err
}

// regionKnown reports whether the scope has the region: a stored row, or a non-retired server of it.
func regionKnown(ctx context.Context, tx *sql.Tx, scope Scope, slug string) (known bool, name string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT coalesce((SELECT display_name FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2 AND slug=$3),$3),
 EXISTS(SELECT 1 FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2 AND slug=$3)
 OR EXISTS(SELECT 1 FROM (`+regionNodesSQL+`) x WHERE region=$3)`, scope.Kind, scope.ID, slug).Scan(&name, &known)
	return known, name, err
}

// RenameRegion changes only the display name of region id; the slug stays. An implicit region is stored first.
func (s *Service) RenameRegion(ctx context.Context, actor string, scope Scope, id, name string) (Region, error) {
	name, err := validRegionName(name)
	if err != nil {
		return Region{}, err
	}
	if !validRegion(id) {
		return Region{}, ErrRegionNotFound
	}
	scope, err = s.regionScope(ctx, actor, scope, true)
	if err != nil {
		return Region{}, err
	}
	err = s.withRegionTx(ctx, actor, scope, func(tx *sql.Tx) error {
		known, _, err := regionKnown(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if !known {
			return ErrRegionNotFound
		}
		// Another region with this name (stored, or implicit under its slug) refuses; the region itself keeps its name.
		taken, err := readRegionTaken(ctx, tx, scope)
		if err != nil {
			return err
		}
		if owner, found := taken.nameOwner(name, id); found {
			return regionExists(owner)
		}
		var stored, count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE slug=$3),count(*) FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2`, scope.Kind, scope.ID, id).Scan(&stored, &count); err != nil {
			return err
		}
		if stored == 0 && count >= regionLimit {
			return regionLimitError()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO paperboat.selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES($1,$2,$3,$4)
ON CONFLICT(scope_kind,scope_id,slug) DO UPDATE SET display_name=excluded.display_name,updated_at=now()`, scope.Kind, scope.ID, id, name)
		return regionConflict(err, name)
	})
	if err != nil {
		return Region{}, err
	}
	regions, err := s.ListRegions(ctx, actor, scope)
	if err != nil {
		return Region{}, err
	}
	for _, r := range regions {
		if r.ID == id {
			return r, nil
		}
	}
	return Region{ID: id, Name: name}, nil
}

// DeleteRegion removes the stored region id. It refuses while a server of the scope is enrolled in it or a workspace's
// pair names it: the owner removes or moves those first.
func (s *Service) DeleteRegion(ctx context.Context, actor string, scope Scope, id string) error {
	if !validRegion(id) {
		return ErrRegionNotFound
	}
	scope, err := s.regionScope(ctx, actor, scope, true)
	if err != nil {
		return err
	}
	return s.withRegionTx(ctx, actor, scope, func(tx *sql.Tx) error {
		known, name, err := regionKnown(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if !known {
			return ErrRegionNotFound
		}
		var machines, pairs bool
		// Servers: any non-retired node of the scope in the region, claimed or not. Its pool entry goes with it.
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (`+regionNodesSQL+`) x WHERE region=$3),
 EXISTS(SELECT 1 FROM paperboat.workspace_edge_sets s WHERE s.scope_kind=$1 AND s.scope_id=$2 AND $3=ANY(s.regions))
  OR ($1='team' AND EXISTS(SELECT 1 FROM paperboat.workspace_edge_sets s WHERE s.scope_kind='account' AND s.mode='self-hosted-only' AND $3=ANY(s.regions) AND paperboat.selfhost_scope_visible($1,$2,s.scope_id)
   AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_regions o WHERE o.scope_kind='account' AND o.scope_id=s.scope_id AND o.slug=$3)
   AND NOT EXISTS(SELECT 1 FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.scope_kind='account' AND i.scope_id=s.scope_id AND n.region=$3)))
  OR EXISTS(SELECT 1 FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id JOIN paperboat.selfhost_pool_policies p ON i.id=ANY(p.installation_ids)
   WHERE i.scope_kind=$1 AND i.scope_id=$2 AND n.region=$3)`,
			scope.Kind, scope.ID, id).Scan(&machines, &pairs); err != nil {
			return err
		}
		if machines {
			return &regionError{message: fmt.Sprintf("%s still has machines. Remove them (or drain and remove them) first, then remove the region.", name), base: ErrRegionInUse}
		}
		if pairs {
			return &regionError{message: fmt.Sprintf("Workspaces still use %s. Switch their regions first, then remove it.", name), base: ErrRegionInUse}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM paperboat.selfhost_regions WHERE scope_kind=$1 AND scope_id=$2 AND slug=$3`, scope.Kind, scope.ID, id)
		return err
	})
}
