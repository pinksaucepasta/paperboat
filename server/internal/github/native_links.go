package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

const NativeLinkLifetime = 10 * time.Minute

var (
	ErrNativeLink = errors.New("GitHub connection request is unavailable")
	// ErrNativeLinkOtherAccount: the signed-in console user is not the link's account.
	ErrNativeLinkOtherAccount = errors.New("GitHub connection request belongs to another account")
	// ErrNativeLinkExpired: the request's ten minutes have passed.
	ErrNativeLinkExpired = errors.New("GitHub connection request has expired")
	// ErrNativeLinkUsed: the request was already approved or cancelled.
	ErrNativeLinkUsed = errors.New("GitHub connection request was already used")
)

// NativeLinkReview is what the console shows before the user approves a CLI's request.
type NativeLinkReview struct {
	AccountEmail string    `json:"account_email"`
	MachineName  string    `json:"machine_name"`
	RequestedAt  time.Time `json:"requested_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type NativeLink struct {
	ID                  string    `json:"id"`
	State               string    `json:"state"`
	ExpiresAt           time.Time `json:"expires_at"`
	BrowserURL          string    `json:"browser_url,omitempty"`
	PollIntervalSeconds int       `json:"poll_interval_seconds,omitempty"`
}

func nativeHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

var nativeLinkID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func nativeSecret() string {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

const nativeActiveClient = `SELECT s.id FROM paperboat.cli_client_sessions s JOIN paperboat.users u ON u.id=s.user_id WHERE s.id=$1 AND s.user_id=$2 AND s.state='active' AND s.revoked_at IS NULL AND 'machines:connect'=ANY(s.scopes) AND u.status='active' FOR UPDATE OF s`

// ConsoleOrigin is the console that hosts the GitHub connect and result pages (the
// console URL /v1/client-configuration publishes).
func (s *Service) ConsoleOrigin() string {
	return strings.TrimRight(s.cfg.HTTP.BrowserLoginOrigin, "/")
}

func (s *Service) StartNativeLink(ctx context.Context, user, client string) (NativeLink, error) {
	if _, err := s.OAuthAuthorizeURL("check", s.DefaultCallbackURL()); err != nil {
		return NativeLink{}, err
	}
	if s.ConsoleOrigin() == "" {
		return NativeLink{}, ErrClientNotConfigured
	}
	item := NativeLink{ID: newID("handoff"), State: "pending", ExpiresAt: s.now().UTC().Add(NativeLinkLifetime), PollIntervalSeconds: 2}
	launch := nativeSecret()
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&id); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM paperboat.github_native_links WHERE id IN (SELECT id FROM paperboat.github_native_links WHERE expires_at<$1 ORDER BY expires_at LIMIT 200)`, s.now().Add(-24*time.Hour)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO paperboat.github_native_links(id,user_id,client_session_id,launch_hash,state,expires_at,created_at) VALUES($1,$2,$3,$4,'pending',$5,$6) ON CONFLICT(client_session_id) DO UPDATE SET id=excluded.id,user_id=excluded.user_id,launch_hash=excluded.launch_hash,browser_hash='',state_hash='',state='pending',expires_at=excluded.expires_at,created_at=excluded.created_at`, item.ID, user, client, nativeHash(launch), item.ExpiresAt, s.now().UTC())
		return err
	})
	if err != nil {
		return NativeLink{}, err
	}
	// The token rides in the fragment: it never reaches a server log or a link previewer,
	// and the console page reads it to review, approve or cancel the request.
	item.BrowserURL = s.ConsoleOrigin() + "/github/connect?link=" + url.QueryEscape(item.ID) + "#" + launch
	return item, nil
}
func (s *Service) NativeLinkStatus(ctx context.Context, user, client, id string, cancel bool) (NativeLink, error) {
	var item NativeLink
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var active string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		if cancel {
			if _, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state='canceled' WHERE id=$1 AND user_id=$2 AND client_session_id=$3 AND state IN('pending','authorizing','exchanging')`, id, user, client); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT id,state,expires_at FROM paperboat.github_native_links WHERE id=$1 AND user_id=$2 AND client_session_id=$3`, id, user, client).Scan(&item.ID, &item.State, &item.ExpiresAt); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		return nil
	})
	if err == nil && !s.now().Before(item.ExpiresAt) && item.State != "completed" && item.State != "canceled" && item.State != "failed" {
		item.State = "expired"
	}
	return item, err
}

// ReviewNativeLink shows the signed-in console user the CLI's request. It changes nothing,
// so a reload, a link preview or a second tab never spends the request.
func (s *Service) ReviewNativeLink(ctx context.Context, viewer, id, token string) (NativeLinkReview, error) {
	var review NativeLinkReview
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		_, err := s.pendingNativeLink(ctx, tx, viewer, id, token)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT u.primary_email,coalesce(m.alias,''),l.created_at,l.expires_at FROM paperboat.github_native_links l
 JOIN paperboat.users u ON u.id=l.user_id
 JOIN paperboat.cli_client_sessions c ON c.id=l.client_session_id
 LEFT JOIN paperboat.user_machines m ON m.id=c.user_machine_id
 WHERE l.id=$1`, id).Scan(&review.AccountEmail, &review.MachineName, &review.RequestedAt, &review.ExpiresAt)
	})
	return review, err
}

// ApproveNativeLink spends the request once and returns where the browser goes to authorize
// GitHub; the OAuth state is bound to this request on the server, so no browser cookie is needed.
func (s *Service) ApproveNativeLink(ctx context.Context, viewer, id, token string) (string, error) {
	state := "pbnative_" + id + "." + nativeSecret()
	if err := s.decideNativeLink(ctx, viewer, id, token, "authorizing", nativeHash(state)); err != nil {
		return "", err
	}
	return s.OAuthAuthorizeURL(state, s.DefaultCallbackURL())
}

// CancelNativeLink spends the request without connecting; the CLI's poll sees canceled.
func (s *Service) CancelNativeLink(ctx context.Context, viewer, id, token string) error {
	return s.decideNativeLink(ctx, viewer, id, token, "canceled", "")
}

func (s *Service) decideNativeLink(ctx context.Context, viewer, id, token, target, stateHash string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := s.pendingNativeLink(ctx, tx, viewer, id, token); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state=$1,state_hash=$2 WHERE id=$3 AND state='pending'`, target, stateHash, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNativeLinkUsed
		}
		return nil
	})
}

// pendingNativeLink locks the request and checks, in order, that it exists for this token,
// belongs to the viewer, is still pending and has not expired, and that the CLI session that
// started it is still active.
func (s *Service) pendingNativeLink(ctx context.Context, tx *db.Tx, viewer, id, token string) (string, error) {
	if !nativeLinkID.MatchString(id) || token == "" {
		return "", ErrNativeLink
	}
	var user, client, launchHash, state string
	var expires time.Time
	if err := tx.QueryRow(ctx, `SELECT user_id,client_session_id,launch_hash,state,expires_at FROM paperboat.github_native_links WHERE id=$1 FOR UPDATE`, id).Scan(&user, &client, &launchHash, &state, &expires); err != nil {
		return "", nativeLinkUnavailableRow(err)
	}
	if launchHash == "" || subtle.ConstantTimeCompare([]byte(launchHash), []byte(nativeHash(token))) != 1 {
		return "", ErrNativeLink
	}
	if user != viewer {
		return "", ErrNativeLinkOtherAccount
	}
	if state != "pending" {
		return "", ErrNativeLinkUsed
	}
	if !s.now().Before(expires) {
		return "", ErrNativeLinkExpired
	}
	var active string
	if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
		return "", nativeLinkUnavailableRow(err)
	}
	return client, nil
}

func NativeStateID(state string) string {
	if !strings.HasPrefix(state, "pbnative_") {
		return ""
	}
	id, secret, ok := strings.Cut(strings.TrimPrefix(state, "pbnative_"), ".")
	if !ok || secret == "" || len(state) > 256 || !nativeLinkID.MatchString(id) {
		return ""
	}
	return id
}
func (s *Service) CompleteNativeLink(ctx context.Context, state, code string) error {
	id := NativeStateID(state)
	if id == "" {
		return ErrNativeLink
	}
	var user, client string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT user_id,client_session_id FROM paperboat.github_native_links WHERE id=$1`, id).Scan(&user, &client); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		var active string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state='exchanging' WHERE id=$1 AND state='authorizing' AND state_hash=$2 AND expires_at>$3`, id, nativeHash(state), s.now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNativeLink
		}
		return nil
	})
	if err != nil {
		return err
	}
	if code == "" {
		if _, cleanupErr := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.github_native_links SET state='failed',browser_hash='',state_hash='' WHERE id=$1 AND state='exchanging'`, id); cleanupErr != nil {
			return errors.Join(ErrNativeLink, cleanupErr)
		}
		return ErrNativeLink
	}
	_, err = s.completeOAuth(ctx, user, code, s.DefaultCallbackURL(), func(ctx context.Context, tx *db.Tx) error {
		var active string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state='completed',browser_hash='',state_hash='' WHERE id=$1 AND user_id=$2 AND client_session_id=$3 AND state='exchanging' AND expires_at>$4`, id, user, client, s.now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNativeLink
		}
		return nil
	})
	if err != nil {
		// Provider linking is committed before best-effort provisioning/status.
		// A later status failure must not report a committed link as failed.
		var completed bool
		queryErr := s.db.SQL().QueryRowContext(ctx, `SELECT state='completed' FROM paperboat.github_native_links WHERE id=$1 AND user_id=$2 AND client_session_id=$3`, id, user, client).Scan(&completed)
		if queryErr == nil && completed {
			supportreport.ObserveFromContext(ctx, "paperboat-server", "provider_github", "service_error", "github_connection_unavailable", err)
			return nil
		}
		_, cleanupErr := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.github_native_links SET state='failed',browser_hash='',state_hash='' WHERE id=$1 AND state='exchanging'`, id)
		if queryErr != nil && queryErr != sql.ErrNoRows {
			return errors.Join(err, queryErr, cleanupErr)
		}
		if cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
	}
	return err
}

// NativeLinkLogin is the GitHub login connected by a completed CLI request, for the console's
// result page. It is empty when the request is unknown or the login was not recorded.
func (s *Service) NativeLinkLogin(ctx context.Context, state string) (string, error) {
	var login string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT t.provider_account_login FROM paperboat.github_native_links l
		JOIN paperboat.github_oauth_tokens t ON t.user_id=l.user_id AND t.revoked_at IS NULL
		WHERE l.id=$1 AND l.state='completed' ORDER BY t.updated_at DESC LIMIT 1`, NativeStateID(state)).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return login, err
}

func nativeLinkUnavailableRow(err error) error {
	if err == pgx.ErrNoRows {
		return ErrNativeLink
	}
	return err
}
