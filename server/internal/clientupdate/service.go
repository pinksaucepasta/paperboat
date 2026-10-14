// Package clientupdate owns the supported CLI protocol floor. It does not
// select artifacts or override the updater's signed activation/recovery floor.
package clientupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/releases"
)

const Schema = "paperboat.client-update-policy/v1"

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

type Policy struct {
	Schema         string     `json:"schema"`
	Revision       int64      `json:"revision"`
	MinimumVersion string     `json:"minimum_version"`
	Reason         string     `json:"reason"`
	EnforceAt      *time.Time `json:"enforce_at,omitempty"`
}
type Input struct {
	ExpectedRevision int64      `json:"expected_revision"`
	MinimumVersion   string     `json:"minimum_version"`
	Reason           string     `json:"reason"`
	EnforceAt        *time.Time `json:"enforce_at"`
}
type Service struct {
	db      *db.DB
	audit   *audit.Writer
	ceiling func() (string, error)
}

func New(store *db.DB, writer *audit.Writer, directory string) *Service {
	return &Service{db: store, audit: writer, ceiling: func() (string, error) {
		current, err := releases.ReadCurrent(directory)
		if err != nil {
			return "", err
		}
		return publishedCeiling(current)
	}}
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
func (p Policy) RequiresUpdate(version string, now time.Time) bool {
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

const selectPolicy = `SELECT revision, minimum_version, reason, enforce_at FROM paperboat.client_update_policy WHERE singleton = true`

func scanPolicy(row pgx.Row) (Policy, error) {
	p := Policy{Schema: Schema}
	e := row.Scan(&p.Revision, &p.MinimumVersion, &p.Reason, &p.EnforceAt)
	return p, e
}
func (s *Service) Get(ctx context.Context) (Policy, error) {
	return scanPolicy(s.db.Pool().QueryRow(ctx, selectPolicy))
}
func (s *Service) Put(ctx context.Context, actor, key, evidence string, in Input) (Policy, error) {
	if err := validate(in); err != nil {
		return Policy{}, err
	}
	// Include the evidence reference: a retry cannot silently rewrite operator intent.
	raw, _ := json.Marshal(struct {
		Input    Input
		Evidence string
	}{in, evidence})
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	keySum := sha256.Sum256([]byte(key))
	keyDigest := hex.EncodeToString(keySum[:])
	var out Policy
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		p, e := scanPolicy(tx.QueryRow(ctx, selectPolicy+` FOR UPDATE`))
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
		if _, e = tx.Exec(ctx, `UPDATE paperboat.client_update_policy SET revision=$1, minimum_version=$2, reason=$3, enforce_at=$4 WHERE singleton=true`, out.Revision, out.MinimumVersion, out.Reason, out.EnforceAt); e != nil {
			return e
		}
		if s.audit == nil {
			return errors.New("policy mutation audit unavailable")
		}
		if e = s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "client_update_policy.changed", ResourceType: "client_update_policy", ResourceID: "client-update-policy", Metadata: map[string]any{"revision": out.Revision, "minimum_version": out.MinimumVersion, "reason": out.Reason, "enforce_at": out.EnforceAt, "evidence_reference": evidence, "idempotency_digest": keyDigest}}); e != nil {
			return e
		}
		response, e = json.Marshal(out)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO paperboat.client_update_policy_requests(actor_id,key_digest,request_digest,response,expires_at) VALUES($1,$2,$3,$4,now()+interval '7 days')`, actor, keyDigest, digest, response)
		return e
	})
	return out, err
}
