// Package clientupdate owns the update floor: the minimum supported version of pb (the CLI) and of the
// node components pbh, edge and relay. It does not select artifacts or override the updater's signed
// activation/recovery floor.
package clientupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/releases"
	"github.com/pinksaucepasta/paperboat/shared/clientupdate"
)

const Schema = clientupdate.Schema

var (
	ErrFloorUnavailable   = errors.New("global floor unavailable on all published platforms")
	ErrInvalid            = errors.New("invalid client update policy")
	ErrConflict           = errors.New("client update policy revision conflict")
	ErrIdempotency        = errors.New("idempotency key reused with different input")
	ErrReleaseUnavailable = errors.New("published release unavailable")
)

type releaseUnavailableFailure struct{ cause error }

func (releaseUnavailableFailure) Error() string { return ErrReleaseUnavailable.Error() }
func (e releaseUnavailableFailure) Unwrap() []error {
	return []error{ErrReleaseUnavailable, e.cause}
}

type Policy = clientupdate.Policy
type Input struct {
	ExpectedRevision int64      `json:"expected_revision"`
	MinimumVersion   string     `json:"minimum_version"`
	Reason           string     `json:"reason"`
	EnforceAt        *time.Time `json:"enforce_at"`
}

// Components are the binaries an update floor covers: pb and the node components.
var Components = []string{"pb", "pbh", "edge", "relay"}

type Service struct {
	component  string
	db         *db.DB
	audit      *audit.Writer
	ceiling    func() (string, error)
	cacheMu    sync.Mutex
	coldMu     sync.Mutex // serializes the first read, when nothing is cached
	refreshing atomic.Bool
	cached     *Policy
	checkedAt  time.Time
}

// New serves pb's floor; the ceiling is the release published for every pb platform.
func New(store *db.DB, writer *audit.Writer, directory string) *Service {
	return &Service{component: "pb", db: store, audit: writer, ceiling: func() (string, error) {
		current, err := releases.ReadCurrent(directory)
		if err != nil {
			return "", err
		}
		return publishedCeiling(current)
	}}
}

// NewNode serves the floor of pbh, edge or relay. They ship together in the self-host release, so the
// ceiling is the newest self-host version published for every Linux architecture.
func NewNode(store *db.DB, writer *audit.Writer, directory, component string) *Service {
	return &Service{component: component, db: store, audit: writer, ceiling: func() (string, error) { return publishedSelfhostCeiling(directory) }}
}

// Latest is the newest release published for this component (every platform of it).
func (s *Service) Latest() (string, error) { return s.ceiling() }

// publishedSelfhostCeiling is the newest selfhost/versions/<v> holding the package of every architecture.
func publishedSelfhostCeiling(directory string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "selfhost", "versions"))
	if err != nil {
		return "", releaseUnavailableFailure{cause: err}
	}
	var newest string
	var value [4]uint64
	for _, entry := range entries {
		version, ok := ParseVersion(entry.Name())
		if !ok || newest != "" && Compare(version, value) <= 0 {
			continue
		}
		complete := true
		for _, arch := range []string{"amd64", "arm64"} {
			if _, err := os.Stat(filepath.Join(directory, "selfhost", "versions", entry.Name(), "paperboat-selfhost-linux-"+arch+".tar.gz")); err != nil {
				complete = false
			}
		}
		if complete {
			newest, value = entry.Name(), version
		}
	}
	if newest == "" {
		return "", ErrReleaseUnavailable
	}
	return newest, nil
}

// publishedCeiling requires an installable candidate on every published platform.
// Current.Version is only the newest asset and cannot establish a global floor.
func publishedCeiling(current releases.Current) (string, error) {
	if len(current.Assets) == 0 {
		return "", ErrReleaseUnavailable
	}
	var minimum string
	var value [4]uint64
	for _, asset := range current.Assets {
		version, ok := ParseVersion(asset.Version)
		if !ok {
			return "", ErrReleaseUnavailable
		}
		if minimum == "" || Compare(version, value) < 0 {
			minimum, value = asset.Version, version
		}
	}
	return minimum, nil
}

// ParseVersion compares numeric YYYY.M.D.build versions and rejects invalid
// calendar dates, suffixes, signs, and components too large to compare safely.
func ParseVersion(s string) ([4]uint64, bool) {
	var v [4]uint64
	if len(s) > 64 {
		return v, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return v, false
	}
	for i, p := range parts {
		if p == "" {
			return v, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
		}
		n, e := strconv.ParseUint(p, 10, 32)
		if e != nil {
			return v, false
		}
		v[i] = n
	}
	if v[0] < 1 || v[0] > 9999 || v[1] < 1 || v[1] > 12 || v[2] < 1 || v[2] > 31 {
		return v, false
	}
	d := time.Date(int(v[0]), time.Month(v[1]), int(v[2]), 0, 0, 0, 0, time.UTC)
	return v, d.Year() == int(v[0]) && d.Month() == time.Month(v[1]) && d.Day() == int(v[2])
}
func Compare(a, b [4]uint64) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// RequiresUpdate is the server gate: strict calendar versions, and an unparseable version is behind.
func RequiresUpdate(p Policy, version string, now time.Time) bool {
	if p.MinimumVersion == "" || p.EnforceAt != nil && now.Before(*p.EnforceAt) {
		return false
	}
	floor, valid := ParseVersion(p.MinimumVersion)
	if !valid {
		return true
	}
	current, valid := ParseVersion(version)
	return !valid || Compare(current, floor) < 0
}
func validate(in Input) error {
	if in.ExpectedRevision < 0 || len(in.Reason) > 256 {
		return ErrInvalid
	}
	for _, c := range in.Reason {
		if unicode.IsControl(c) {
			return ErrInvalid
		}
	}
	if in.MinimumVersion != "" {
		if _, ok := ParseVersion(in.MinimumVersion); !ok {
			return ErrInvalid
		}
		if strings.TrimSpace(in.Reason) == "" {
			return ErrInvalid
		}
	}
	if in.MinimumVersion == "" && (in.Reason != "" || in.EnforceAt != nil) {
		return ErrInvalid
	}
	if in.EnforceAt != nil && (in.EnforceAt.Year() < 1 || in.EnforceAt.Year() > 9999) {
		return ErrInvalid
	}
	return nil
}

const selectPolicy = `SELECT revision, minimum_version, reason, enforce_at FROM paperboat.client_update_policy WHERE component = $1`

func scanPolicy(row pgx.Row) (Policy, error) {
	p := Policy{Schema: Schema}
	e := row.Scan(&p.Revision, &p.MinimumVersion, &p.Reason, &p.EnforceAt)
	return p, e
}

// Get answers from a cache of the last good policy. A stale cache is served at once while one background read
// refreshes it, so a slow or failed database never stalls the gate; only the first read (nothing cached) waits,
// and concurrent first reads coalesce into one. Another instance's Put reaches this one within
// PolicyRefreshInterval.
func (s *Service) Get(ctx context.Context) (Policy, error) {
	if p, fresh, ok := s.cachedPolicy(); ok {
		if !fresh && s.refreshing.CompareAndSwap(false, true) {
			go s.refresh()
		}
		return p, nil
	}
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	if p, _, ok := s.cachedPolicy(); ok {
		return p, nil
	}
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}
	return s.read(ctx)
}

func (s *Service) cachedPolicy() (p Policy, fresh, ok bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cached == nil {
		return Policy{}, false, false
	}
	return clonePolicy(*s.cached), time.Since(s.checkedAt) < PolicyRefreshInterval, true
}

func (s *Service) refresh() {
	defer s.refreshing.Store(false)
	// Failed refreshes back off too: an outage must not turn every request into another database read, and a
	// known floor stays enforced indefinitely.
	if _, err := s.read(context.Background()); err != nil {
		s.cacheMu.Lock()
		s.checkedAt = time.Now()
		s.cacheMu.Unlock()
	}
}

func (s *Service) read(ctx context.Context) (Policy, error) {
	read, cancel := context.WithTimeout(ctx, policyReadTimeout)
	defer cancel()
	p, err := scanPolicy(s.db.Pool().QueryRow(read, selectPolicy, s.component))
	if err != nil {
		return Policy{}, err
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.rememberPolicy(p)
	return clonePolicy(*s.cached), nil
}
func (s *Service) Put(ctx context.Context, actor, key, evidence string, in Input) (Policy, error) {
	if err := validate(in); err != nil {
		return Policy{}, err
	}
	// Include the evidence reference: a retry cannot silently rewrite operator intent.
	raw, _ := json.Marshal(struct {
		Component string
		Input     Input
		Evidence  string
	}{s.component, in, evidence})
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	keySum := sha256.Sum256([]byte(key))
	keyDigest := hex.EncodeToString(keySum[:])
	var out Policy
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, e := scanPolicy(tx.QueryRow(ctx, selectPolicy+` FOR UPDATE`, s.component))
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `DELETE FROM paperboat.client_update_policy_requests WHERE expires_at < now()`)
		if e != nil {
			return e
		}
		var oldDigest string
		var response []byte
		e = tx.QueryRow(ctx, `SELECT request_digest, response FROM paperboat.client_update_policy_requests WHERE actor_id=$1 AND key_digest=$2`, actor, keyDigest).Scan(&oldDigest, &response)
		if e == nil {
			if oldDigest != digest {
				return ErrIdempotency
			}
			return json.Unmarshal(response, &out)
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if p.Revision != in.ExpectedRevision {
			return ErrConflict
		}
		if in.MinimumVersion != "" {
			latest, e := s.ceiling()
			if e != nil {
				if e == ErrReleaseUnavailable {
					return e
				}
				return releaseUnavailableFailure{cause: e}
			}
			published, ok := ParseVersion(latest)
			if !ok {
				return ErrReleaseUnavailable
			}
			floor, _ := ParseVersion(in.MinimumVersion)
			if Compare(floor, published) > 0 {
				return ErrFloorUnavailable
			}
		}
		out = Policy{Schema: Schema, Revision: p.Revision + 1, MinimumVersion: in.MinimumVersion, Reason: in.Reason, EnforceAt: in.EnforceAt}
		if _, e = tx.Exec(ctx, `UPDATE paperboat.client_update_policy SET revision=$1, minimum_version=$2, reason=$3, enforce_at=$4 WHERE component=$5`, out.Revision, out.MinimumVersion, out.Reason, out.EnforceAt, s.component); e != nil {
			return e
		}
		if s.audit == nil {
			return errors.New("policy mutation audit unavailable")
		}
		if e = s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "client_update_policy.changed", ResourceType: "client_update_policy", ResourceID: "client-update-policy", Metadata: map[string]any{"component": s.component, "revision": out.Revision, "minimum_version": out.MinimumVersion, "reason": out.Reason, "enforce_at": out.EnforceAt, "evidence_reference": evidence, "idempotency_digest": keyDigest}}); e != nil {
			return e
		}
		response, e = json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO paperboat.client_update_policy_requests(actor_id,key_digest,request_digest,response,expires_at) VALUES($1,$2,$3,$4,now()+interval '7 days')`, actor, keyDigest, digest, response)
		return e
	})
	if err == nil {
		s.cacheMu.Lock()
		s.rememberPolicy(out)
		s.cacheMu.Unlock()
	}
	return out, err
}
