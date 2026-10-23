package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// TestUndecryptableTokenIsDisconnected proves that when the stored GitHub token
// was encrypted under an encryption key that is no longer configured (key
// drift), token-dependent operations surface ErrNotConnected and status renders
// disconnected so the dashboard prompts the user to reconnect.
func TestUndecryptableTokenIsDisconnected(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run Postgres integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_gh_keydrift_" + suffix
	if _, err := store.SQL().ExecContext(ctx,
		`INSERT INTO paperboat.users (id, workos_subject, primary_email, status)
		 VALUES ($1, $2, $3, 'active')`,
		userID, "workos_gh_keydrift_"+suffix, "gh-keydrift-"+suffix+"@example.com"); err != nil {
		t.Fatal(err)
	}

	// Token encrypted under the original key.
	ciphertext, err := secrets.Encrypt("original-encryption-key", "gho_realtoken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx,
		`INSERT INTO paperboat.github_oauth_tokens
		   (user_id, token_ciphertext, scopes, provider_account_login, last_validated_at, created_at, updated_at)
		 VALUES ($1, $2, ARRAY['repo']::text[], $3, now(), now(), now())`,
		userID, ciphertext, "octocat"); err != nil {
		t.Fatal(err)
	}

	// The server now runs with a different encryption key.
	cfg := config.Config{}
	cfg.Secrets.EncryptionKey = "a-different-encryption-key-after-rotation"
	cfg.GitHub.OAuthScopes = []string{"repo"}
	service := NewService(store, audit.NewWriter(store), &FakeClient{}, cfg)

	if _, err := service.ListRepos(ctx, userID); err != ErrNotConnected {
		t.Fatalf("ListRepos error = %v, want ErrNotConnected", err)
	}
	status, err := service.Status(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Connected || len(status.Scopes) != 0 || len(status.MissingScopes) != 0 {
		t.Fatalf("status = %#v, want disconnected empty status", status)
	}
}

type timeoutAfterCreateClient struct {
	FakeClient
	timedOut bool
}

func (c *timeoutAfterCreateClient) CreateRepo(ctx context.Context, token string, input RepoCreateInput) (Repo, error) {
	repo, err := c.FakeClient.CreateRepo(ctx, token, input)
	if err != nil {
		return Repo{}, err
	}
	if !c.timedOut {
		c.timedOut = true
		return Repo{}, context.DeadlineExceeded
	}
	return repo, nil
}

func TestProvisionConfigRepoReconcilesTimeoutAfterCreate(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run Postgres integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_gh_uncertain_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "gh-uncertain-"+suffix+"@example.com"); err != nil {
		t.Fatal(err)
	}
	encryptionKey := "github-uncertain-test-key"
	token, err := secrets.Encrypt(encryptionKey, "gho_timeout_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.github_oauth_tokens (user_id,token_ciphertext,scopes,provider_account_login,last_validated_at,created_at,updated_at) VALUES ($1,$2,ARRAY['repo']::text[],$3,now(),now(),now())`, userID, token, "octocat"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Secrets.EncryptionKey = encryptionKey
	cfg.GitHub.OAuthScopes = []string{"repo"}
	cfg.GitHub.ConfigRepoName = "paperboat-config"
	cfg.GitHub.ConfigRepoBranch = "main"
	client := &timeoutAfterCreateClient{FakeClient: FakeClient{User: GitHubUser{Login: "octocat"}}}
	service := NewService(store, audit.NewWriter(store), client, cfg)
	key := "github-provision-" + suffix

	if _, err := service.ProvisionConfigRepo(ctx, userID, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first provision error = %v, want deadline exceeded", err)
	}
	var state string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.github_repo_provisioning_attempts WHERE user_id=$1 AND idempotency_key=$2`, userID, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "uncertain" {
		t.Fatalf("attempt state = %q, want uncertain", state)
	}
	repo, err := service.ProvisionConfigRepo(ctx, userID, key)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Owner != "octocat" || repo.Name != "paperboat-config" || client.Created != 1 {
		t.Fatalf("repo = %#v, creates = %d", repo, client.Created)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.github_repo_provisioning_attempts WHERE user_id=$1 AND idempotency_key=$2`, userID, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" {
		t.Fatalf("retry state = %q, want succeeded", state)
	}
	var repositoryID string
	if err := store.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.control_config_repositories WHERE owner_user_id=$1 AND provider='github' AND external_repository_id=$2 AND provisioned_at IS NOT NULL`, userID, repo.ID).Scan(&repositoryID); err != nil {
		t.Fatal(err)
	}
	assignments := controlplane.NewConfigAssignmentService(store, audit.NewWriter(store), "")
	assignments.SetRepositoryResolver(controlplane.ConfigRepositoryResolverFunc(func(ctx context.Context, user, provider, externalID string) (controlplane.ConfigRepositoryConnection, error) {
		resolved, account, err := service.ResolvePrivateRepository(ctx, user, externalID)
		return controlplane.ConfigRepositoryConnection{ProviderAccountID: account, ExternalRepositoryID: resolved.ID, DisplayName: resolved.Owner + "/" + resolved.Name, CloneURL: resolved.CloneURL, PublishURL: resolved.CloneURL, DefaultBranch: resolved.DefaultBranch, AuthorizationRef: "github-user:" + user, CredentialCapability: "provider_user_repository"}, err
	}))
	connected, err := assignments.ConnectRepository(ctx, userID, "github", repo.ID, "")
	if err != nil || connected.ID != repositoryID {
		t.Fatalf("canonical reconnect = %q, %v", connected.ID, err)
	}
	status, err := service.Status(ctx, userID)
	if err != nil || !status.ConfigRepoProvisioned || status.ConfigRepoOwner != repo.Owner || status.ConfigRepoName != repo.Name || status.ConfigRepoBranch != repo.DefaultBranch {
		t.Fatalf("provisioned status = %+v, %v", status, err)
	}
	otherUser := userID + "_other"
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, otherUser, otherUser+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := service.recordAttempt(ctx, otherUser, key, "started", "other", "paperboat-config", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.github_repo_provisioning_attempts WHERE user_id=$1 AND idempotency_key=$2`, userID, key).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("provisioning receipt crossed owner: %q, %v", state, err)
	}
}
