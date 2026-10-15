package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

var ErrNativeLink = errors.New("GitHub connection request is unavailable")

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

// NativeBrowserOrigin is the origin hosting native confirmation pages. It is
// deliberately independent of the provider callback contract.
func (s *Service) NativeBrowserOrigin() string {
	public, err := url.Parse(s.cfg.HTTP.PublicBaseURL)
	if err != nil || public.Scheme == "" || public.Host == "" {
		return ""
	}
	return public.Scheme + "://" + public.Host
}

func (s *Service) StartNativeLink(ctx context.Context, user, client string) (NativeLink, error) {
	if _, err := s.OAuthAuthorizeURL("check", s.DefaultCallbackURL()); err != nil {
		return NativeLink{}, err
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
	item.BrowserURL = strings.TrimRight(s.cfg.HTTP.PublicBaseURL, "/") + "/v1/github/native-links/" + item.ID + "/launch?token=" + url.QueryEscape(launch)
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

// LaunchNativeLink consumes the one-use launch capability and binds the operation to the
// browser before showing its explicit account confirmation page.
func (s *Service) LaunchNativeLink(ctx context.Context, id, token, browser string) (string, error) {
	var email string
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var user, client string
		if err := tx.QueryRow(ctx, `SELECT user_id,client_session_id FROM paperboat.github_native_links WHERE id=$1`, id).Scan(&user, &client); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		var active string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET launch_hash='',browser_hash=$1 WHERE id=$2 AND launch_hash=$3 AND state='pending' AND expires_at>$4`, nativeHash(browser), id, nativeHash(token), s.now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNativeLink
		}
		return nativeLinkUnavailableRow(tx.QueryRow(ctx, `SELECT primary_email FROM paperboat.users WHERE id=$1`, user).Scan(&email))
	})
	return email, err
}
func (s *Service) AuthorizeNativeLink(ctx context.Context, id, browser string, cancel bool) (string, error) {
	state := "pbnative_" + id + "." + nativeSecret()
	target := "authorizing"
	if cancel {
		target = "canceled"
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var user, client string
		if err := tx.QueryRow(ctx, `SELECT user_id,client_session_id FROM paperboat.github_native_links WHERE id=$1`, id).Scan(&user, &client); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		var active string
		if err := tx.QueryRow(ctx, nativeActiveClient, client, user).Scan(&active); err != nil {
			return nativeLinkUnavailableRow(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state=$1,state_hash=$2 WHERE id=$3 AND browser_hash=$4 AND state='pending' AND launch_hash='' AND expires_at>$5`, target, nativeHash(state), id, nativeHash(browser), s.now().UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNativeLink
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if cancel {
		return "", nil
	}
	return s.OAuthAuthorizeURL(state, s.DefaultCallbackURL())
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
func (s *Service) CompleteNativeLink(ctx context.Context, state, browser, code string) error {
	id := NativeStateID(state)
	if id == "" || browser == "" {
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
		tag, err := tx.Exec(ctx, `UPDATE paperboat.github_native_links SET state='exchanging' WHERE id=$1 AND state='authorizing' AND state_hash=$2 AND browser_hash=$3 AND expires_at>$4`, id, nativeHash(state), nativeHash(browser), s.now().UTC())
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

func nativeLinkUnavailableRow(err error) error {
	if err == pgx.ErrNoRows {
		return ErrNativeLink
	}
	return err
}
