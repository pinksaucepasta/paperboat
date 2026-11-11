// Package teambucket keeps each team's own S3-compatible bucket: where it is and which credentials reach it (stored
// encrypted, never returned). Machines archive their terminal recordings in it. Everything in the bucket is client-side
// ciphertext; machines and browsers get short-lived presigned requests from here.
package teambucket

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/auditstream"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

var (
	ErrForbidden = errors.New("your team role cannot manage the team bucket")
	ErrNotFound  = errors.New("team or bucket not found")
	ErrNeedsTeam = errors.New("the team bucket is a Team plan feature")
	ErrNoKey     = errors.New("the server has no encryption key for bucket credentials")
)

type Service struct {
	db     *db.DB
	key    string
	now    func() time.Time
	client func() *http.Client
}

func New(store *db.DB, encryptionKey string) *Service {
	return &Service{db: store, key: encryptionKey, now: time.Now, client: func() *http.Client { return auditstream.PublicClient(20 * time.Second) }}
}

// authorizeTx checks the team, its Team plan and the account's permission to manage settings.
func authorizeTx(ctx context.Context, tx *db.Tx, account, team string) error {
	if err := teams.Lock(ctx, tx); err != nil {
		return err
	}
	var role *string
	var paid bool
	err := tx.QueryRow(ctx, `SELECT (SELECT CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END FROM team_members m WHERE m.team_id=t.team_id AND m.account_id=$2 AND m.active),team_paid_entitled(t.team_id)
 FROM teams t WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended`, team, account).Scan(&role, &paid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && role == nil {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !teams.Allowed(*role, teams.PermissionSettingsManage) {
		return ErrForbidden
	}
	if !paid {
		return ErrNeedsTeam
	}
	return teams.SSOGuardTx(ctx, tx, team, account)
}

const columns = `endpoint, region, bucket, prefix, path_style, access_key_id, checked_at`

func scan(row interface{ Scan(...any) error }) (shared.Config, error) {
	var c shared.Config
	err := row.Scan(&c.Endpoint, &c.Region, &c.Bucket, &c.Prefix, &c.PathStyle, &c.AccessKeyID, &c.CheckedAt)
	return c, err
}

// Get is GET /v1/teams/{team}/bucket (owners and admins). A team without a bucket gets ErrNotFound.
func (s *Service) Get(ctx context.Context, account, team string) (shared.Config, error) {
	var out shared.Config
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team); err != nil {
			return err
		}
		var err error
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM team_buckets WHERE team_id=$1`, team))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return out, err
}

// Put is PUT /v1/teams/{team}/bucket. A new or changed bucket, key or secret is checked with a probe object first and
// refused with a *ProbeError naming the failing step; saving the same bucket again changes nothing.
func (s *Service) Put(ctx context.Context, account, team string, in shared.Input) (shared.Config, error) {
	if s.key == "" {
		return shared.Config{}, ErrNoKey
	}
	if err := in.Validate(); err != nil {
		return shared.Config{}, err
	}
	var current *shared.Config
	var sealed []byte
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team); err != nil {
			return err
		}
		c, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM team_buckets WHERE team_id=$1`, team))
		if err == nil {
			current = &c
			return tx.QueryRow(ctx, `SELECT secret FROM team_buckets WHERE team_id=$1`, team).Scan(&sealed)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return shared.Config{}, err
	}
	secret := in.SecretAccessKey
	if secret == "" {
		if current == nil {
			return shared.Config{}, shared.ErrInvalid
		}
		if secret, err = secrets.Decrypt(s.key, sealed); err != nil {
			return shared.Config{}, err
		}
	}
	next := shared.Config{Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket, Prefix: in.Prefix, PathStyle: in.PathStyle,
		AccessKeyID: in.AccessKeyID, CheckedAt: s.now().UTC()}
	if current != nil && in.SecretAccessKey == "" && sameBucket(*current, next) {
		return *current, nil // nothing changed: the bucket was already proved
	}
	store, err := newStore(ctx, next, secret, s.client())
	if err != nil {
		return shared.Config{}, err
	}
	if err := store.Probe(ctx); err != nil {
		return shared.Config{}, err
	}
	box, err := secrets.Encrypt(s.key, secret)
	if err != nil {
		return shared.Config{}, err
	}
	var out shared.Config
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO team_buckets(team_id,endpoint,region,bucket,prefix,path_style,access_key_id,secret,checked_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(team_id) DO UPDATE SET endpoint=EXCLUDED.endpoint,region=EXCLUDED.region,bucket=EXCLUDED.bucket,prefix=EXCLUDED.prefix,path_style=EXCLUDED.path_style,
 access_key_id=EXCLUDED.access_key_id,secret=EXCLUDED.secret,checked_at=EXCLUDED.checked_at,updated_at=now()`,
			team, next.Endpoint, next.Region, next.Bucket, next.Prefix, next.PathStyle, next.AccessKeyID, box, next.CheckedAt); err != nil {
			return err
		}
		out = next
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "team_bucket_configured", map[string]any{})
	})
	return out, err
}

func sameBucket(a, b shared.Config) bool {
	return a.Endpoint == b.Endpoint && a.Region == b.Region && a.Bucket == b.Bucket && a.Prefix == b.Prefix && a.PathStyle == b.PathStyle && a.AccessKeyID == b.AccessKeyID
}

// Delete is DELETE /v1/teams/{team}/bucket. Objects already in the bucket stay: they are the team's.
func (s *Service) Delete(ctx context.Context, account, team string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM team_buckets WHERE team_id=$1`, team)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "team_bucket_removed", map[string]any{})
	})
}

// StoreFor opens team's bucket inside the caller's transaction: ErrNotFound when the team has none.
func (s *Service) StoreFor(ctx context.Context, tx *db.Tx, team string) (ObjectStore, error) {
	if s.key == "" {
		return nil, ErrNoKey
	}
	var sealed []byte
	var c shared.Config
	err := tx.QueryRow(ctx, `SELECT `+columns+`, secret FROM team_buckets WHERE team_id=$1`, team).
		Scan(&c.Endpoint, &c.Region, &c.Bucket, &c.Prefix, &c.PathStyle, &c.AccessKeyID, &c.CheckedAt, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	secret, err := secrets.Decrypt(s.key, sealed)
	if err != nil {
		return nil, err
	}
	store, err := newStore(ctx, c, secret, s.client())
	if err != nil {
		return nil, err
	}
	return store, nil
}
