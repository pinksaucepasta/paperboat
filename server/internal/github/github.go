package github

import (
	"context"
	"github.com/google/uuid"

	"database/sql"

	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type OAuthToken struct {
	AccessToken      string
	RefreshToken     string
	Scopes           []string
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
}

type Client interface {
	ExchangeOAuthCode(context.Context, OAuthExchangeInput) (OAuthToken, error)
	RefreshOAuthToken(context.Context, OAuthRefreshInput) (OAuthToken, error)
	CurrentUser(context.Context, string) (GitHubUser, error)
	GetRepo(context.Context, string, string, string) (Repo, error)
	ListRepos(context.Context, string) ([]Repo, error)
	CreateRepo(context.Context, string, RepoCreateInput) (Repo, error)
	GetFile(context.Context, string, string, string, string, string) (File, error)
	PutFile(context.Context, string, string, string, PutFileInput) error
}

type OAuthRefreshInput struct {
	RefreshToken string
	ClientID     string
	ClientSecret string
}

type OAuthExchangeInput struct {
	Code         string
	RedirectURI  string
	ClientID     string
	ClientSecret string
}

type GitHubUser struct {
	Login string
}

type Repo struct {
	ID            string
	Owner         string
	Name          string
	DefaultBranch string
	CloneURL      string
	HTMLURL       string
	Private       bool
}

type RepoCreateInput struct {
	Name          string
	Private       bool
	AutoInit      bool
	DefaultBranch string
}

type PutFileInput struct {
	Path    string
	Message string
	Content []byte
	Branch  string
}

type File struct {
	Path string
	SHA  string
}

type Status struct {
	Connected bool `json:"connected"`
	// Login and AvatarURL name the connected GitHub account (absent when not connected).
	Login                 string    `json:"login,omitempty"`
	AvatarURL             string    `json:"avatar_url,omitempty"`
	Scopes                []string  `json:"scopes"`
	MissingScopes         []string  `json:"missing_scopes"`
	LastValidatedAt       time.Time `json:"last_validated_at,omitempty"`
	ConfigRepoProvisioned bool      `json:"config_repo_provisioned"`
	ConfigRepoOwner       string    `json:"config_repo_owner,omitempty"`
	ConfigRepoName        string    `json:"config_repo_name,omitempty"`
	ConfigRepoBranch      string    `json:"config_repo_branch,omitempty"`
}

func (s Status) normalized() Status {
	if s.Scopes == nil {
		s.Scopes = []string{}
	}
	if s.MissingScopes == nil {
		s.MissingScopes = []string{}
	}
	return s
}

type ConfigRepo struct {
	ID            string `json:"id"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	Private       bool   `json:"private"`
}

type Service struct {
	db     *db.DB
	audit  *audit.Writer
	client Client
	cfg    config.Config
	now    func() time.Time
}

type RepositoryAccess struct {
	Token     string
	ExpiresAt time.Time
}

func NewService(store *db.DB, auditWriter *audit.Writer, client Client, cfg config.Config) *Service {
	return &Service{db: store, audit: auditWriter, client: client, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) DefaultCallbackURL() string {
	return strings.TrimRight(s.cfg.HTTP.PublicBaseURL, "/") + "/v1/github/oauth/callback"
}

func (s *Service) OAuthAuthorizeURL(state, redirectURI string) (string, error) {
	if strings.TrimSpace(s.cfg.Secrets.GitHubClientID) == "" {
		return "", ErrClientNotConfigured
	}
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = s.DefaultCallbackURL()
	}
	u, err := url.Parse(s.cfg.GitHub.OAuthAuthorizeURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", s.cfg.Secrets.GitHubClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	if scopes := s.requiredOAuthScopes(); len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Service) CompleteOAuth(ctx context.Context, userID, code, redirectURI string) (Status, error) {
	return s.completeOAuth(ctx, userID, code, redirectURI, nil)
}

func (s *Service) completeOAuth(ctx context.Context, userID, code, redirectURI string, guard func(context.Context, *db.Tx) error) (Status, error) {
	if s.client == nil {
		return Status{}, ErrClientNotConfigured
	}
	token, err := s.client.ExchangeOAuthCode(ctx, OAuthExchangeInput{
		Code:         code,
		RedirectURI:  redirectURI,
		ClientID:     s.cfg.Secrets.GitHubClientID,
		ClientSecret: s.cfg.Secrets.GitHubClientSecret,
	})
	if err != nil {
		return Status{}, err
	}
	missing := missingScopes(token.Scopes, s.requiredOAuthScopes())
	if len(missing) > 0 {
		return Status{Connected: false, Scopes: token.Scopes, MissingScopes: missing}.normalized(), ErrMissingScopes
	}
	ghUser, err := s.client.CurrentUser(ctx, token.AccessToken)
	if err != nil {
		return Status{}, err
	}
	accessCiphertext, err := secrets.Encrypt(s.cfg.Secrets.EncryptionKey, token.AccessToken)
	if err != nil {
		return Status{}, err
	}
	var refreshCiphertext []byte
	if token.RefreshToken != "" {
		refreshCiphertext, err = secrets.Encrypt(s.cfg.Secrets.EncryptionKey, token.RefreshToken)
		if err != nil {
			return Status{}, err
		}
	}
	var expiresAt sql.NullTime
	if !token.ExpiresAt.IsZero() {
		expiresAt = sql.NullTime{Time: token.ExpiresAt, Valid: true}
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		q := tx.Queries()
		linkedUserID, err := q.LinkGitHubIdentity(ctx, dbsqlc.LinkGitHubIdentityParams{UserID: userID, ProviderSubject: ghUser.Login})
		if err != nil {
			return err
		}
		if linkedUserID != userID {
			return ErrIdentityLinkedToAnotherUser
		}
		err = q.UpsertGitHubOAuthToken(ctx, dbsqlc.UpsertGitHubOAuthTokenParams{UserID: userID, TokenCiphertext: accessCiphertext, RefreshTokenCiphertext: refreshCiphertext, Scopes: token.Scopes, ExpiresAt: expiresAt, ProviderAccountLogin: ghUser.Login})
		if err != nil {
			return err
		}
		var refreshExpiresAt sql.NullTime
		if !token.RefreshExpiresAt.IsZero() {
			refreshExpiresAt = sql.NullTime{Time: token.RefreshExpiresAt, Valid: true}
		}
		if _, err := tx.Exec(ctx, `UPDATE paperboat.github_oauth_tokens SET refresh_expires_at=$2 WHERE user_id=$1`, userID, refreshExpiresAt); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{
			ActorUserID: userID, ActorType: audit.ActorUser, EventType: "github.oauth.connected",
			ResourceType: "user", ResourceID: userID, IdempotencyKey: "github.oauth.connected:" + userID + ":" + ghUser.Login,
			Metadata: map[string]any{"provider": "github", "scopes": token.Scopes},
		})
	})
	if err != nil {
		return Status{}, err
	}
	// Provision the private config repository as part of connecting GitHub so
	// setup completes in a single step and the user never has to trigger it
	// separately. This is best-effort: a transient GitHub failure must not fail
	// the connection (it can be retried), so the error is intentionally not
	// propagated — the returned Status reflects whether provisioning succeeded
	// (config_repo_provisioned), and the dashboard keeps a manual retry path for
	// the rare failure case.
	_, provisionErr := s.ProvisionConfigRepo(ctx, userID, "github.config_repo.connect:"+userID)
	status, statusErr := s.Status(ctx, userID)
	if statusErr != nil {
		if provisionErr != nil {
			statusErr = errors.Join(statusErr, provisionErr)
		}
		return Status{}, statusErr
	}
	if provisionErr != nil {
		supportreport.ObserveFromContext(ctx, "paperboat-server", "provider_github", "provider_call", "github_connection_unavailable", provisionErr)
	}
	return status, nil
}

func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	var status Status
	if _, _, _, err := s.githubTokenWithExpiry(ctx, userID); errors.Is(err, ErrNotConnected) {
		return status.normalized(), nil
	} else if err != nil && !errors.Is(err, ErrMissingScopes) {
		return Status{}, err
	}
	connection, err := s.db.Queries().GetGitHubConnectionReadiness(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return status.normalized(), nil
	}
	if err != nil {
		return Status{}, err
	}
	if _, err := secrets.Decrypt(s.cfg.Secrets.EncryptionKey, connection.TokenCiphertext); errors.Is(err, secrets.ErrDecrypt) {
		return status.normalized(), nil
	} else if err != nil {
		return Status{}, err
	}
	status.Connected = true
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT provider_account_login FROM paperboat.github_oauth_tokens WHERE user_id=$1 AND revoked_at IS NULL ORDER BY updated_at DESC LIMIT 1`, userID).Scan(&status.Login); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Status{}, err
	}
	status.AvatarURL = s.avatarURL(status.Login)
	status.Scopes = connection.Scopes
	status.LastValidatedAt = connection.LastValidatedAt.Time
	status.MissingScopes = missingScopes(connection.Scopes, s.requiredOAuthScopes())
	repo, err := s.db.Queries().GetGitHubConfigRepoStatus(ctx, userID)
	if err == nil {
		status.ConfigRepoOwner, status.ConfigRepoName, status.ConfigRepoBranch = repo.Owner.String, repo.Name, repo.DefaultBranch.String
		status.ConfigRepoProvisioned = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Status{}, err
	}
	return status.normalized(), nil
}

func (s *Service) ProvisionConfigRepo(ctx context.Context, userID, idempotencyKey string) (ConfigRepo, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return ConfigRepo{}, ErrIdempotencyKeyRequired
	}
	accessToken, owner, err := s.githubToken(ctx, userID)
	if err != nil {
		return ConfigRepo{}, err
	}
	if s.client == nil {
		return ConfigRepo{}, errors.New("github client is not configured")
	}
	repoName := s.cfg.GitHub.ConfigRepoName
	branch := s.cfg.GitHub.ConfigRepoBranch
	if err := s.recordAttempt(ctx, userID, idempotencyKey, "started", owner, repoName, ""); err != nil {
		return ConfigRepo{}, err
	}
	repo, err := s.client.GetRepo(ctx, accessToken, owner, repoName)
	if errors.Is(err, ErrRepoNotFound) {
		repo, err = s.client.CreateRepo(ctx, accessToken, RepoCreateInput{Name: repoName, Private: true, AutoInit: true, DefaultBranch: branch})
	}
	if err != nil {
		state := "retryable_failed"
		if githubOutcomeUncertain(err) {
			state = "uncertain"
		}
		if attemptErr := s.recordAttempt(ctx, userID, idempotencyKey, state, owner, repoName, githubProvisioningErrorSummary(ctx, err)); attemptErr != nil {
			err = errors.Join(err, attemptErr)
		}
		return ConfigRepo{}, err
	}
	if !repo.Private {
		err := errors.New("github config repository is not private")
		if attemptErr := s.recordAttempt(ctx, userID, idempotencyKey, "failed", owner, repoName, githubProvisioningErrorSummary(ctx, err)); attemptErr != nil {
			err = errors.Join(err, attemptErr)
		}
		return ConfigRepo{}, err
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = branch
	}
	if err := s.initializeRepo(ctx, userID, accessToken, repo); err != nil {
		state := "retryable_failed"
		if githubOutcomeUncertain(err) {
			state = "uncertain"
		}
		if attemptErr := s.recordAttempt(ctx, userID, idempotencyKey, state, owner, repoName, githubProvisioningErrorSummary(ctx, err)); attemptErr != nil {
			err = errors.Join(err, attemptErr)
		}
		return ConfigRepo{}, err
	}
	if err := s.storeRepo(ctx, userID, idempotencyKey, repo); err != nil {
		return ConfigRepo{}, err
	}
	return ConfigRepo(repo), nil
}

func githubOutcomeUncertain(err error) bool {
	var apiErr APIError
	return !errors.As(err, &apiErr) || apiErr.StatusCode >= http.StatusInternalServerError
}

// ListRepos returns the repositories the connected GitHub account can access,
// most-recently-updated first, for selection during project creation.
func (s *Service) ListRepos(ctx context.Context, userID string) ([]Repo, error) {
	token, _, err := s.githubToken(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.client.ListRepos(ctx, token)
}

// ResolvePrivateRepository resolves an immutable provider repository ID through
// the user's current authorization. Coordinates supplied by callers are never
// trusted, and public or no-longer-accessible repositories are rejected.
func (s *Service) ResolvePrivateRepository(ctx context.Context, userID, externalRepositoryID string) (Repo, string, error) {
	externalRepositoryID = strings.TrimSpace(externalRepositoryID)
	if externalRepositoryID == "" {
		return Repo{}, "", ErrRepoNotFound
	}
	token, accountLogin, err := s.githubToken(ctx, userID)
	if err != nil {
		return Repo{}, "", err
	}
	repository, err := s.repositoryByID(ctx, token, externalRepositoryID)
	if err != nil {
		return Repo{}, "", err
	}
	if !repository.Private || repository.CloneURL == "" || repository.DefaultBranch == "" {
		return Repo{}, "", ErrRepoNotPrivate
	}
	return repository, accountLogin, nil
}

// IssueUserRepositoryAccess leases the connected user's current provider
// credential to an authenticated config worker after confirming the selected
// private repository remains visible. Provider permissions and branch rules
// remain authoritative; contentsPermission cannot elevate the credential.
func (s *Service) IssueUserRepositoryAccess(ctx context.Context, userID, repositoryID, contentsPermission string) (RepositoryAccess, error) {
	if contentsPermission != "read" && contentsPermission != "write" {
		return RepositoryAccess{}, ErrRepositoryAccessUnavailable
	}
	token, _, providerExpiry, err := s.githubTokenWithExpiry(ctx, userID)
	if err != nil {
		return RepositoryAccess{}, err
	}
	repository, err := s.repositoryByID(ctx, token, repositoryID)
	if err != nil {
		return RepositoryAccess{}, err
	}
	if !repository.Private || repository.CloneURL == "" || repository.DefaultBranch == "" {
		return RepositoryAccess{}, ErrRepoNotFound
	}
	expiresAt := s.now().UTC().Add(15 * time.Minute)
	if !providerExpiry.IsZero() && providerExpiry.Before(expiresAt) {
		expiresAt = providerExpiry
	}
	return RepositoryAccess{Token: token, ExpiresAt: expiresAt}, nil
}

func (s *Service) githubToken(ctx context.Context, userID string) (string, string, error) {
	token, login, _, err := s.githubTokenWithExpiry(ctx, userID)
	return token, login, err
}

func (s *Service) githubTokenWithExpiry(ctx context.Context, userID string) (string, string, time.Time, error) {
	row, err := s.db.Queries().GetGitHubToken(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, ErrNotConnected
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	if missing := missingScopes(row.Scopes, s.requiredOAuthScopes()); len(missing) > 0 {
		return "", "", time.Time{}, ErrMissingScopes
	}
	if row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(s.now().UTC().Add(time.Minute)) {
		return s.refreshGitHubToken(ctx, userID)
	}
	token, err := secrets.Decrypt(s.cfg.Secrets.EncryptionKey, row.TokenCiphertext)
	if errors.Is(err, secrets.ErrDecrypt) {
		// The stored token was encrypted under an encryption key that is no
		// longer configured, so it can't be used. This is a broken credential,
		// not a provider outage — surface it as "not connected" so callers
		// prompt the user to reconnect GitHub, which re-encrypts the token under
		// the current key.
		return "", "", time.Time{}, ErrNotConnected
	}
	expiresAt := time.Time{}
	if row.ExpiresAt.Valid {
		expiresAt = row.ExpiresAt.Time.UTC()
	}
	return token, row.ProviderAccountLogin, expiresAt, err
}

// GitHub App user access tokens are authorized by the App's configured
// permissions and do not carry classic OAuth scopes. Classic OAuth
// integrations still require the configured scopes.
func (s *Service) requiredOAuthScopes() []string {
	if strings.TrimSpace(s.cfg.GitHub.AppID) != "" {
		return nil
	}
	return s.cfg.GitHub.OAuthScopes
}

func (s *Service) initializeRepo(ctx context.Context, _ string, token string, repo Repo) error {
	files := map[string][]byte{
		".pbinclude":                      {},
		".paperboat/preview-url-skill.md": []byte("# Preview URLs\n\nWhen an app starts on localhost inside a Paperboat VM, surface the Paperboat preview URL instead of a raw localhost URL.\n"),
		".paperboat/config-sync.json":     []byte("{\n  \"schema_version\": 2,\n  \"revision\": \"2\"\n}\n"),
	}
	for path, content := range files {
		if _, err := s.client.GetFile(ctx, token, repo.Owner, repo.Name, path, repo.DefaultBranch); err == nil {
			continue
		} else if !errors.Is(err, ErrFileNotFound) {
			return err
		}
		if err := s.client.PutFile(ctx, token, repo.Owner, repo.Name, PutFileInput{
			Path: path, Message: "Initialize Paperboat config", Content: content, Branch: repo.DefaultBranch,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recordAttempt(ctx context.Context, userID, idempotencyKey, state, owner, name, lastErr string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return tx.Queries().UpsertGitHubProvisioningAttempt(ctx, dbsqlc.UpsertGitHubProvisioningAttemptParams{UserID: userID, IdempotencyKey: idempotencyKey, State: state, RepoOwner: owner, RepoName: name, LastError: lastErr})
	})
}

func githubProvisioningErrorSummary(ctx context.Context, err error) string {
	fault := supportreport.ProjectFault(ctx, "paperboat-server", "provider_github", "provider_call", "github_connection_unavailable", err)
	parts := []string{"cause=" + fault.Cause, "error_type=" + fault.ErrorType}
	if fault.HTTPStatus != 0 {
		parts = append(parts, "http_status="+strconv.Itoa(fault.HTTPStatus))
	}
	if fault.Errno != 0 {
		parts = append(parts, "errno="+strconv.Itoa(fault.Errno))
	}
	if len(fault.ErrorChain) > 0 {
		chain := fault.ErrorChain
		if len(chain) > 2 {
			chain = chain[:2]
		}
		parts = append(parts, "error_chain="+strings.Join(chain, ","))
	}
	return strings.Join(parts, " ")
}

func (s *Service) storeRepo(ctx context.Context, userID, idempotencyKey string, repo Repo) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		q := tx.Queries()
		repositoryID, err := q.UpsertGitHubConfigRepository(ctx, dbsqlc.UpsertGitHubConfigRepositoryParams{ID: newID("repository"), UserID: userID, ProviderRepoID: repo.ID, Owner: sql.NullString{String: repo.Owner, Valid: true}, DisplayName: repo.Owner + "/" + repo.Name, DefaultBranch: sql.NullString{String: repo.DefaultBranch, Valid: true}, CloneUrl: sql.NullString{String: repo.CloneURL, Valid: true}, AuthorizationRef: sql.NullString{String: "github-user:" + userID, Valid: true}})
		if err != nil {
			return err
		}
		if err := q.MarkGitHubProvisioningSucceeded(ctx, dbsqlc.MarkGitHubProvisioningSucceededParams{UserID: userID, IdempotencyKey: idempotencyKey}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{
			ActorUserID: userID, ActorType: audit.ActorUser, EventType: "github.config_repo.provisioned",
			ResourceType: "config_repository", ResourceID: repositoryID,
			IdempotencyKey: "github.config_repo.provisioned:" + userID + ":" + repo.Owner + "/" + repo.Name,
			Metadata:       map[string]any{"provider": "github", "private": repo.Private, "default_branch": repo.DefaultBranch},
		})
	})
}

type HTTPClient struct {
	BaseURL  string
	TokenURL string
	Client   *http.Client
}

func (c HTTPClient) ExchangeOAuthCode(ctx context.Context, input OAuthExchangeInput) (token OAuthToken, retErr error) {
	form := url.Values{}
	form.Set("client_id", input.ClientID)
	form.Set("client_secret", input.ClientSecret)
	form.Set("code", input.Code)
	form.Set("redirect_uri", input.RedirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthToken{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.httpClient().Do(req)
	if err != nil {
		return OAuthToken{}, err
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return OAuthToken{}, APIError{StatusCode: res.StatusCode}
	}
	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Scope            string `json:"scope"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return OAuthToken{}, err
	}
	token = OAuthToken{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken, Scopes: splitScopes(body.Scope)}
	if body.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().UTC().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.RefreshExpiresIn > 0 && body.RefreshExpiresIn <= 31536000 {
		token.RefreshExpiresAt = time.Now().UTC().Add(time.Duration(body.RefreshExpiresIn) * time.Second)
	}
	return token, nil
}

func (c HTTPClient) CurrentUser(ctx context.Context, token string) (GitHubUser, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return GitHubUser{}, err
	}
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return GitHubUser{}, normalizeGitHubOAuthError(err)
	}
	return GitHubUser{Login: user.GetLogin()}, nil
}

func (c HTTPClient) GetRepo(ctx context.Context, token, owner, name string) (Repo, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return Repo{}, err
	}
	repository, _, err := client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return Repo{}, normalizeGitHubError(err)
	}
	return repoFromSDK(repository), nil
}

func (c HTTPClient) ListRepos(ctx context.Context, token string) ([]Repo, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return nil, err
	}
	repos := make([]Repo, 0, 100)
	for page := 1; ; page++ {
		items, response, err := client.Repositories.ListByAuthenticatedUser(ctx, &gh.RepositoryListByAuthenticatedUserOptions{
			Affiliation: "owner,collaborator,organization_member",
			Sort:        "updated",
			ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, normalizeGitHubError(err)
		}
		for _, item := range items {
			repos = append(repos, repoFromSDK(item))
		}
		if response == nil || response.NextPage == 0 {
			break
		}
	}
	return repos, nil
}

func (c HTTPClient) CreateRepo(ctx context.Context, token string, input RepoCreateInput) (Repo, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return Repo{}, err
	}
	repository, _, err := client.Repositories.Create(ctx, "", &gh.Repository{
		Name:          gh.Ptr(input.Name),
		Private:       gh.Ptr(input.Private),
		AutoInit:      gh.Ptr(input.AutoInit),
		DefaultBranch: gh.Ptr(input.DefaultBranch),
	})
	if err != nil {
		return Repo{}, normalizeGitHubError(err)
	}
	repo := repoFromSDK(repository)
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = input.DefaultBranch
	}
	return repo, nil
}

func (c HTTPClient) GetFile(ctx context.Context, token, owner, name, path, branch string) (File, error) {
	client, err := c.sdkClient(token)
	if err != nil {
		return File{}, err
	}
	options := &gh.RepositoryContentGetOptions{Ref: branch}
	content, directory, _, err := client.Repositories.GetContents(ctx, owner, name, path, options)
	if err != nil {
		err = normalizeGitHubError(err)
		if errors.Is(err, ErrRepoNotFound) {
			return File{}, ErrFileNotFound
		}
		return File{}, err
	}
	if content == nil || len(directory) != 0 || (content.GetType() != "" && content.GetType() != "file") {
		return File{}, fmt.Errorf("github path %q exists but is not a file", path)
	}
	return File{Path: content.GetPath(), SHA: content.GetSHA()}, nil
}

func (c HTTPClient) PutFile(ctx context.Context, token, owner, name string, input PutFileInput) error {
	client, err := c.sdkClient(token)
	if err != nil {
		return err
	}
	_, _, err = client.Repositories.CreateFile(ctx, owner, name, input.Path, &gh.RepositoryContentFileOptions{
		Message: gh.Ptr(input.Message),
		Content: input.Content,
		Branch:  gh.Ptr(input.Branch),
	})
	err = normalizeGitHubError(err)
	if isAlreadyExistsValidation(err) {
		return nil
	}
	return err
}

func (c HTTPClient) sdkClient(token string) (*gh.Client, error) {
	options := []gh.ClientOptionsFunc{gh.WithHTTPClient(c.httpClient())}
	if token != "" {
		options = append(options, gh.WithAuthToken(token))
	}
	if strings.TrimSpace(c.BaseURL) != "" && strings.TrimRight(c.BaseURL, "/") != "https://api.github.com" {
		base, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/")
		if err != nil || base.Scheme == "" || base.Host == "" {
			return nil, errors.New("github API base URL is invalid")
		}
		baseURL := base.String()
		options = append(options, gh.WithURLs(&baseURL, &baseURL))
	}
	client, err := gh.NewClient(options...)
	if err != nil {
		return nil, fmt.Errorf("create github client: %w", err)
	}
	return client, nil
}

func repoFromSDK(repository *gh.Repository) Repo {
	if repository == nil {
		return Repo{}
	}
	id := ""
	if repository.ID != nil {
		id = strconv.FormatInt(repository.GetID(), 10)
	}
	return Repo{
		ID: id, Owner: repository.GetOwner().GetLogin(), Name: repository.GetName(),
		DefaultBranch: repository.GetDefaultBranch(), CloneURL: repository.GetCloneURL(),
		HTMLURL: repository.GetHTMLURL(), Private: repository.GetPrivate(),
	}
}

func normalizeGitHubError(err error) error {
	if err == nil {
		return nil
	}
	var response *gh.ErrorResponse
	if !errors.As(err, &response) || response.Response == nil {
		return err
	}
	if response.Response.StatusCode == http.StatusNotFound {
		return ErrRepoNotFound
	}
	details := make([]APIErrorDetail, 0, len(response.Errors))
	for _, detail := range response.Errors {
		details = append(details, APIErrorDetail{
			Resource: detail.Resource, Field: detail.Field, Code: detail.Code, Message: detail.Message,
		})
	}
	return APIError{StatusCode: response.Response.StatusCode, Message: response.Message, Errors: details}
}

func normalizeGitHubOAuthError(err error) error {
	if err == nil {
		return nil
	}
	response, ok := err.(*gh.ErrorResponse)
	if !ok || response == nil || response.Response == nil {
		return err
	}
	return APIError{StatusCode: response.Response.StatusCode, Cause: err}
}

type APIError struct {
	StatusCode int
	Message    string
	Errors     []APIErrorDetail
	Cause      error
}

func (e APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github api request failed: status %d", e.StatusCode)
	}
	return fmt.Sprintf("github api request failed: status %d: %s", e.StatusCode, e.Message)
}

func (e APIError) DiagnosticStatus() int { return e.StatusCode }

func (e APIError) Unwrap() error { return e.Cause }

func (e APIError) OAuthCredentialRejected() bool {
	if e.StatusCode != http.StatusBadRequest && e.StatusCode != http.StatusUnauthorized {
		return false
	}
	if e.Cause == nil {
		return true
	}
	_, directProviderResponse := e.Cause.(*gh.ErrorResponse)
	return directProviderResponse
}

type APIErrorDetail struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func isAlreadyExistsValidation(err error) bool {
	var apiErr APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	for _, detail := range apiErr.Errors {
		if detail.Code == "already_exists" {
			return true
		}
	}
	return false
}

func (c HTTPClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return serverdiagnostics.DefaultProviderClient("github")
}

type FakeClient struct {
	Token       OAuthToken
	User        GitHubUser
	Repos       map[string]Repo
	Created     int
	PutFiles    []string
	ExchangeErr error
	CreateErr   error
}

func (f *FakeClient) ExchangeOAuthCode(_ context.Context, input OAuthExchangeInput) (OAuthToken, error) {
	if f.ExchangeErr != nil {
		return OAuthToken{}, f.ExchangeErr
	}
	if strings.TrimSpace(input.Code) == "" {
		return OAuthToken{}, errors.New("github oauth code is required")
	}
	if f.Token.AccessToken != "" {
		return f.Token, nil
	}
	return OAuthToken{AccessToken: "fake-gh-token", Scopes: []string{"repo"}}, nil
}

func (f *FakeClient) RefreshOAuthToken(context.Context, OAuthRefreshInput) (OAuthToken, error) {
	return f.Token, f.ExchangeErr
}

func (f *FakeClient) CurrentUser(context.Context, string) (GitHubUser, error) {
	if f.User.Login != "" {
		return f.User, nil
	}
	return GitHubUser{Login: "paperboat-test-user"}, nil
}

func (f *FakeClient) GetRepo(_ context.Context, _, owner, name string) (Repo, error) {
	if f.Repos == nil {
		f.Repos = map[string]Repo{}
	}
	repo, ok := f.Repos[owner+"/"+name]
	if !ok {
		return Repo{}, ErrRepoNotFound
	}
	return repo, nil
}

func (f *FakeClient) ListRepos(context.Context, string) ([]Repo, error) {
	repos := make([]Repo, 0, len(f.Repos))
	for _, repo := range f.Repos {
		repos = append(repos, repo)
	}
	slices.SortFunc(repos, func(a, b Repo) int {
		return strings.Compare(a.Owner+"/"+a.Name, b.Owner+"/"+b.Name)
	})
	return repos, nil
}

func (f *FakeClient) CreateRepo(_ context.Context, _ string, input RepoCreateInput) (Repo, error) {
	if f.CreateErr != nil {
		return Repo{}, f.CreateErr
	}
	if f.Repos == nil {
		f.Repos = map[string]Repo{}
	}
	f.Created++
	owner := f.User.Login
	if owner == "" {
		owner = "paperboat-test-user"
	}
	repo := Repo{ID: "fake-repo-id", Owner: owner, Name: input.Name, DefaultBranch: input.DefaultBranch, CloneURL: "https://github.com/" + owner + "/" + input.Name + ".git", HTMLURL: "https://github.com/" + owner + "/" + input.Name, Private: input.Private}
	f.Repos[owner+"/"+input.Name] = repo
	return repo, nil
}

func (f *FakeClient) GetFile(_ context.Context, _, owner, name, path, _ string) (File, error) {
	key := owner + "/" + name + "/" + path
	for _, existing := range f.PutFiles {
		if existing == key {
			return File{Path: path, SHA: "fake-sha"}, nil
		}
	}
	return File{}, ErrFileNotFound
}

func (f *FakeClient) PutFile(_ context.Context, _, owner, name string, input PutFileInput) error {
	f.PutFiles = append(f.PutFiles, owner+"/"+name+"/"+input.Path)
	return nil
}

func missingScopes(have, required []string) []string {
	var missing []string
	for _, scope := range required {
		scope = strings.TrimSpace(scope)
		if scope != "" && !slices.Contains(have, scope) {
			missing = append(missing, scope)
		}
	}
	return missing
}

func splitScopes(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", " ")
	fields := strings.Fields(raw)
	if fields == nil {
		return []string{}
	}
	return fields
}

func newID(prefix string) string { return prefix + "_" + uuid.NewString() }

var (
	ErrClientNotConfigured         = errors.New("github client is not configured")
	ErrNotConnected                = errors.New("github connection required")
	ErrMissingScopes               = errors.New("github oauth token is missing required scopes")
	ErrIdentityLinkedToAnotherUser = errors.New("github identity is already linked to another user")
	ErrRepoNotFound                = errors.New("github repository not found")
	ErrRepoNotPrivate              = errors.New("github repository is not private")
	ErrFileNotFound                = errors.New("github file not found")
	ErrIdempotencyKeyRequired      = errors.New("idempotency key is required")
	ErrRepositoryAccessUnavailable = errors.New("repository-scoped access is unavailable")
)

// ErrConnectionInUse means the GitHub connection cannot be removed while an active config
// repository syncs through it.
var ErrConnectionInUse = errors.New("GitHub connection is used by the config repository")

// avatarURL is the account's public avatar on the configured GitHub web host.
func (s *Service) avatarURL(login string) string {
	if login == "" {
		return ""
	}
	authorize, err := url.Parse(s.cfg.GitHub.OAuthAuthorizeURL)
	if err != nil || authorize.Scheme == "" || authorize.Host == "" {
		return ""
	}
	return authorize.Scheme + "://" + authorize.Host + "/" + url.PathEscape(login) + ".png"
}

// Disconnect revokes the stored GitHub token (Paperboat stops acting on GitHub for the user).
// The grant itself stays listed in the user's GitHub settings until they remove it there.
func (s *Service) Disconnect(ctx context.Context, userID string) (Status, error) {
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var inUse bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.control_config_repositories WHERE owner_user_id=$1 AND provider='github' AND provisioned_at IS NOT NULL AND state='active')`, userID).Scan(&inUse); err != nil {
			return err
		}
		if inUse {
			return ErrConnectionInUse
		}
		_, err := tx.Exec(ctx, `UPDATE paperboat.github_oauth_tokens SET revoked_at=now(),updated_at=now(),version=version+1 WHERE user_id=$1 AND revoked_at IS NULL`, userID)
		return err
	})
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID)
}
