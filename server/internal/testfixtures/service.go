// Package testfixtures is the narrow server-side surface the final re-check's CI runner calls with one dedicated
// token: a paid team for a throwaway account, and temporary DNS records in a confined test domain. It can do nothing
// else: the team calls touch only the team derived from the account's email, the DNS calls only records it created
// itself (marked by a comment) under the configured domain, every call is rate limited and audited as a system actor.
package testfixtures

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/ratelimit"
)

var (
	ErrInvalid         = errors.New("test fixture request is invalid")
	ErrAccountNotFound = errors.New("the account has not signed in yet")
	ErrNotConfigured   = errors.New("test fixture DNS is not configured")
	ErrRecordNotFound  = errors.New("test fixture DNS record not found")
	ErrNotEntitled     = errors.New("the fixture team did not become paid")
	emailPattern       = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}$`)
	maxMembers         = 8
)

// DNS is the Cloudflare zone the test domain lives in.
type DNS interface {
	Create(ctx context.Context, zoneID string, record Record) (string, error)
	Get(ctx context.Context, zoneID, id string) (Record, bool, error)
	Delete(ctx context.Context, zoneID, id string) error
}

type Config struct {
	DB     *db.DB
	Audit  *audit.Writer
	Token  string
	Domain string // records may only be named under this domain, e.g. smoke.example.test
	ZoneID string // the Cloudflare zone that holds Domain
	DNS    DNS
}

type Service struct {
	cfg     Config
	limiter *ratelimit.Limiter
}

func New(cfg Config) (*Service, error) {
	if cfg.Audit == nil || len(cfg.Token) < 32 {
		return nil, errors.New("test fixtures need an audit writer and a token of at least 32 bytes")
	}
	cfg.Domain = strings.ToLower(strings.Trim(cfg.Domain, "."))
	return &Service{cfg: cfg, limiter: ratelimit.New(60, 30, 4)}, nil
}

// Authenticate compares the bearer in constant time.
func (s *Service) Authenticate(token string) bool {
	return s != nil && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) == 1
}

// Allow rate limits all calls together: one runner, one token.
func (s *Service) Allow() (bool, time.Duration) { return s.limiter.Allow("fixtures") }

// TeamName is the one team of an account, stable so create is idempotent and remove finds it.
func TeamName(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return "paperboat-smoke-" + hex.EncodeToString(sum[:])[:12]
}

func (s *Service) record(ctx context.Context, tx *db.Tx, event, team string, meta map[string]any) error {
	return s.cfg.Audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: event, ResourceType: "team", ResourceID: team, Metadata: meta})
}

func accountID(ctx context.Context, tx *db.Tx, email string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(primary_email)=lower($1) AND status='active'`, email).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrAccountNotFound, email)
	}
	return id, err
}

// CreateTeam gives the account a team it owns with the paid entitlement granted as test data (what a completed team
// checkout leaves: team-pro, quantity 10, an active period) and each member as an active member. Idempotent.
func (s *Service) CreateTeam(ctx context.Context, email string, members []string) (string, error) {
	if !emailPattern.MatchString(email) || len(members) > maxMembers {
		return "", ErrInvalid
	}
	for _, m := range members {
		if !emailPattern.MatchString(m) {
			return "", ErrInvalid
		}
	}
	if s.cfg.DB == nil {
		return "", ErrNotConfigured
	}
	team := TeamName(email)
	err := s.cfg.DB.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		owner, err := accountID(ctx, tx, email)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO teams(team_id, owner_account, generation, display_name, approve_new_machines) VALUES ($1,$2,1,$1,true) ON CONFLICT (team_id) DO NOTHING`, team, owner); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO team_members(team_id, account_id, membership_generation, role, active) VALUES ($1,$2,1,'member',true) ON CONFLICT (team_id, account_id) DO NOTHING`, team, owner); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO billing_workspaces(id, team_id, product_code, state, quantity, period_start, period_end, bandwidth_anchor, cancel_at_period_end)
 VALUES ('team:' || $1::text, $1, 'team-pro', 'active', 10, now() - interval '1 hour', now() + interval '90 days', now() - interval '1 hour', false)
 ON CONFLICT (id) DO UPDATE SET state='active', period_start=now() - interval '1 hour', period_end=now() + interval '90 days', cancel_at_period_end=false`, team); err != nil {
			return err
		}
		for _, m := range members {
			member, err := accountID(ctx, tx, m)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO team_members(team_id, account_id, membership_generation, role, active) VALUES ($1,$2,1,'member',true)
 ON CONFLICT (team_id, account_id) DO UPDATE SET membership_generation=team_members.membership_generation+1, role='member', active=true, joined_at=now()`, team, member); err != nil {
				return err
			}
		}
		var paid bool
		if err = tx.QueryRow(ctx, `SELECT team_paid_entitled($1)`, team).Scan(&paid); err != nil {
			return err
		}
		if !paid {
			return ErrNotEntitled
		}
		return s.record(ctx, tx, "test_fixture_team_created", team, map[string]any{"owner": owner, "members": len(members)})
	})
	return team, err
}

// RemoveTeam makes every other member inactive, which is what blocks the owner's account deletion; the product
// deletes a team left with its owner alone when the account is deleted. It returns the members still active.
func (s *Service) RemoveTeam(ctx context.Context, email string) (int, error) {
	if !emailPattern.MatchString(email) {
		return 0, ErrInvalid
	}
	if s.cfg.DB == nil {
		return 0, ErrNotConfigured
	}
	team := TeamName(email)
	var active int
	err := s.cfg.DB.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE team_members m SET active=false, membership_generation=m.membership_generation+1
 FROM teams t WHERE t.team_id=m.team_id AND t.team_id=$1 AND m.account_id<>t.owner_account AND m.active`, team); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_members WHERE team_id=$1 AND active`, team).Scan(&active); err != nil {
			return err
		}
		return s.record(ctx, tx, "test_fixture_team_removed", team, map[string]any{"active_members": active})
	})
	return active, err
}
