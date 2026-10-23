// Package auditstream streams a team's audit log to its SIEM (PLAN F19-10): HTTPS (signed NDJSON
// batches, optional auth header, for Splunk HEC, Datadog, Elastic, Axiom, Cribl, Panther or any
// webhook) and S3-compatible storage (gzip NDJSON objects). Delivery is at least once and in
// order, with a cursor per destination and retries with backoff.
package auditstream

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/activity"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var (
	ErrForbidden  = errors.New("your team role cannot manage audit log streaming")
	ErrNotFound   = errors.New("team or destination not found")
	ErrInvalid    = errors.New("invalid streaming destination")
	ErrNeedsTeam  = errors.New("audit log streaming is a Team plan feature")
	ErrNoKey      = errors.New("the server has no encryption key for destination credentials")
	ErrNoActivity = errors.New("activity log is unavailable")
)

// Destination is one configured stream; secrets are never returned.
type Destination struct {
	Kind            string     `json:"kind"`
	URL             string     `json:"url"`
	HeaderName      string     `json:"header_name,omitempty"`
	Region          string     `json:"region,omitempty"`
	Bucket          string     `json:"bucket,omitempty"`
	Prefix          string     `json:"prefix,omitempty"`
	AccessKeyID     string     `json:"access_key_id,omitempty"`
	LastDeliveredAt *time.Time `json:"last_delivered_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	FailingSince    *time.Time `json:"failing_since,omitempty"`
	Pending         int        `json:"pending"`
	// SigningSecret is returned once, when an HTTPS destination is created.
	SigningSecret string `json:"signing_secret,omitempty"`
	TeamID        string `json:"-"`
	cursor        int64
	attempts      int
}

// Secrets are stored encrypted, never returned.
type Secrets struct {
	SigningSecret   string `json:"signing_secret,omitempty"`
	HeaderValue     string `json:"header_value,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// Input configures a destination; empty secret fields keep the stored ones.
type Input struct {
	URL             string `json:"url"`
	HeaderName      string `json:"header_name,omitempty"`
	HeaderValue     string `json:"header_value,omitempty"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

type Service struct {
	db          *db.DB
	log         *activity.Service
	cfg         config.Business
	key         string
	mail        config.TransactionalMail
	mailEnabled bool
	client      func() httpDoer
	now         func() time.Time
}

func New(store *db.DB, log *activity.Service, cfg config.Business, key string, mail config.TransactionalMail, mailEnabled bool) *Service {
	return &Service{db: store, log: log, cfg: cfg, key: key, mail: mail, mailEnabled: mailEnabled,
		client: func() httpDoer { return publicClient(15 * time.Second) }, now: time.Now}
}

// authorizeTx checks the team, its Team plan and the account's permission.
func authorizeTx(ctx context.Context, tx *db.Tx, account, team string, permission teams.Permission) error {
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
	if !teams.Allowed(*role, permission) {
		return ErrForbidden
	}
	if !paid {
		return ErrNeedsTeam
	}
	return teams.SSOGuardTx(ctx, tx, team, account)
}

const destinationColumns = `kind,url,coalesce(header_name,''),coalesce(region,''),coalesce(bucket,''),coalesce(prefix,''),coalesce(access_key_id,''),last_delivered_at,coalesce(last_error,''),failing_since,cursor_sequence,attempts,team_id`

func scanDestination(row interface{ Scan(...any) error }) (Destination, error) {
	var d Destination
	err := row.Scan(&d.Kind, &d.URL, &d.HeaderName, &d.Region, &d.Bucket, &d.Prefix, &d.AccessKeyID, &d.LastDeliveredAt, &d.LastError, &d.FailingSince, &d.cursor, &d.attempts, &d.TeamID)
	return d, err
}

// List returns the team's destinations with their delivery status (audit.read).
func (s *Service) List(ctx context.Context, account, team string) ([]Destination, error) {
	out := []Destination{}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team, teams.PermissionAuditRead); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+destinationColumns+`,(SELECT count(*) FROM audit_events e WHERE e.activity_workspace_id='team:'||d.team_id AND e.cursor_sequence>d.cursor_sequence)
 FROM audit_stream_destinations d WHERE team_id=$1 ORDER BY kind`, team)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Destination
			if err := rows.Scan(&d.Kind, &d.URL, &d.HeaderName, &d.Region, &d.Bucket, &d.Prefix, &d.AccessKeyID, &d.LastDeliveredAt, &d.LastError, &d.FailingSince, &d.cursor, &d.attempts, &d.TeamID, &d.Pending); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

var (
	headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	region     = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	bucket     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefix     = regexp.MustCompile(`^[A-Za-z0-9!_.*'()/-]{0,256}$`)
	accessKey  = regexp.MustCompile(`^[A-Za-z0-9+/=_.-]{1,128}$`)
)

// validURL accepts an https URL whose host is a name or a public IP literal.
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return publicIP(ip)
	}
	return strings.Contains(u.Hostname(), ".") && !strings.HasSuffix(u.Hostname(), ".internal") && u.Hostname() != "localhost"
}

func (in Input) validate(kind string, creating bool) bool {
	if !validURL(in.URL) || len(in.HeaderValue) > 4096 || len(in.SecretAccessKey) > 256 {
		return false
	}
	switch kind {
	case "https":
		if in.HeaderName == "" {
			return in.HeaderValue == ""
		}
		reserved := map[string]bool{"host": true, "content-type": true, "content-length": true, "pb-signature": true, "user-agent": true}
		return headerName.MatchString(in.HeaderName) && !reserved[strings.ToLower(in.HeaderName)] && (in.HeaderValue != "" || !creating)
	case "s3":
		return region.MatchString(in.Region) && bucket.MatchString(in.Bucket) && prefix.MatchString(in.Prefix) &&
			accessKey.MatchString(in.AccessKeyID) && (in.SecretAccessKey != "" || !creating)
	}
	return false
}

// Configure creates or replaces the team's destination of one kind (audit.stream). A new stream
// starts at the end of the audit log. An HTTPS destination gets a signing secret, returned once.
func (s *Service) Configure(ctx context.Context, account, team, kind string, in Input) (Destination, error) {
	if s.key == "" {
		return Destination{}, ErrNoKey
	}
	var out Destination
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team, teams.PermissionAuditStream); err != nil {
			return err
		}
		var stored []byte
		err := tx.QueryRow(ctx, `SELECT secrets FROM audit_stream_destinations WHERE team_id=$1 AND kind=$2 FOR UPDATE`, team, kind).Scan(&stored)
		creating := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !creating {
			return err
		}
		if !in.validate(kind, creating) {
			return ErrInvalid
		}
		var sec Secrets
		if !creating {
			plain, err := secrets.Decrypt(s.key, stored)
			if err != nil {
				return err
			}
			if err = json.Unmarshal([]byte(plain), &sec); err != nil {
				return err
			}
		}
		if kind == "https" && sec.SigningSecret == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			sec.SigningSecret = "pbsig_" + base64.RawURLEncoding.EncodeToString(raw)
			out.SigningSecret = sec.SigningSecret
		}
		if in.HeaderValue != "" || in.HeaderName == "" {
			sec.HeaderValue = in.HeaderValue
		}
		if in.SecretAccessKey != "" {
			sec.SecretAccessKey = in.SecretAccessKey
		}
		if in.HeaderName != "" && sec.HeaderValue == "" {
			return ErrInvalid // a header added on edit needs its value
		}
		plain, _ := json.Marshal(sec)
		sealed, err := secrets.Encrypt(s.key, string(plain))
		if err != nil {
			return err
		}
		latest, err := s.log.Latest(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_stream_destinations(team_id,kind,url,header_name,region,bucket,prefix,access_key_id,secrets,cursor_sequence)
 VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10)
 ON CONFLICT(team_id,kind) DO UPDATE SET url=EXCLUDED.url,header_name=EXCLUDED.header_name,region=EXCLUDED.region,bucket=EXCLUDED.bucket,
 prefix=EXCLUDED.prefix,access_key_id=EXCLUDED.access_key_id,secrets=EXCLUDED.secrets,next_attempt_at=now(),attempts=0,last_error=NULL,failing_since=NULL,alerted_at=NULL`,
			team, kind, in.URL, in.HeaderName, in.Region, in.Bucket, in.Prefix, in.AccessKeyID, sealed, latest); err != nil {
			return err
		}
		secret := out.SigningSecret
		if out, err = scanDestination(tx.QueryRow(ctx, `SELECT `+destinationColumns+` FROM audit_stream_destinations WHERE team_id=$1 AND kind=$2`, team, kind)); err != nil {
			return err
		}
		out.SigningSecret = secret
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "activity_stream_configured", map[string]any{"resource_kind": kind})
	})
	return out, err
}

// Remove deletes the team's destination of one kind (audit.stream).
func (s *Service) Remove(ctx context.Context, account, team, kind string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team, teams.PermissionAuditStream); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM audit_stream_destinations WHERE team_id=$1 AND kind=$2`, team, kind)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return teams.AuditDirectoryChangeTx(ctx, tx, account, team, "activity_stream_removed", map[string]any{"resource_kind": kind})
	})
}

// Test sends one test event to the destination now and reports the result.
func (s *Service) Test(ctx context.Context, account, team, kind string) error {
	var d Destination
	var sec Secrets
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTx(ctx, tx, account, team, teams.PermissionAuditStream); err != nil {
			return err
		}
		var err error
		d, sec, err = s.loadTx(ctx, tx, team, kind)
		return err
	})
	if err != nil {
		return err
	}
	now := s.now()
	event := map[string]any{"id": "test_" + now.UTC().Format("20060102T150405"), "occurred_at": now.UTC(), "workspace": "team:" + team,
		"action": "team.activity_stream_test", "actor": map[string]any{"kind": "user", "id": account}, "target": map[string]any{"kind": "team", "id": team},
		"result": "success", "details": map[string]any{}}
	line, _ := json.Marshal(event)
	return s.send(ctx, d, sec, append(line, '\n'), 0, 0, now)
}

func (s *Service) loadTx(ctx context.Context, tx *db.Tx, team, kind string) (Destination, Secrets, error) {
	var sec Secrets
	var sealed []byte
	row := tx.QueryRow(ctx, `SELECT `+destinationColumns+`,secrets FROM audit_stream_destinations WHERE team_id=$1 AND kind=$2`, team, kind)
	var d Destination
	err := row.Scan(&d.Kind, &d.URL, &d.HeaderName, &d.Region, &d.Bucket, &d.Prefix, &d.AccessKeyID, &d.LastDeliveredAt, &d.LastError, &d.FailingSince, &d.cursor, &d.attempts, &d.TeamID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, sec, ErrNotFound
	}
	if err != nil {
		return d, sec, err
	}
	plain, err := secrets.Decrypt(s.key, sealed)
	if err != nil {
		return d, sec, err
	}
	return d, sec, json.Unmarshal([]byte(plain), &sec)
}

func (s *Service) send(ctx context.Context, d Destination, sec Secrets, body []byte, first, last int64, now time.Time) error {
	client := s.client()
	if d.Kind == "s3" {
		key := objectKey(d.Prefix, "team:"+d.TeamID, now, first, last)
		if first == 0 {
			key = objectKey(d.Prefix, "team:"+d.TeamID, now, now.Unix(), 0)
		}
		return sendS3(ctx, client, d, sec, body, key, now)
	}
	return sendHTTPS(ctx, client, d, sec, body, now)
}

// record writes a streaming event to the team audit log as the system.
func record(ctx context.Context, tx *db.Tx, team, action string, metadata map[string]any) error {
	return audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "team." + action, ResourceType: "team", ResourceID: team, Metadata: metadata})
}
