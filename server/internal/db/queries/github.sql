-- name: LinkGitHubIdentity :one
INSERT INTO user_identities (user_id, provider, provider_subject, email, created_at, updated_at)
VALUES ($1, 'github', $2, '', now(), now())
ON CONFLICT (provider, provider_subject) DO UPDATE SET updated_at = now()
RETURNING user_id;

-- name: UpsertGitHubOAuthToken :exec
INSERT INTO github_oauth_tokens
	(user_id, token_ciphertext, refresh_token_ciphertext, scopes, expires_at, provider_account_login, last_validated_at, created_at, updated_at)
VALUES (sqlc.arg(user_id), sqlc.arg(token_ciphertext), sqlc.arg(refresh_token_ciphertext), sqlc.arg(scopes), sqlc.narg(expires_at), sqlc.arg(provider_account_login), now(), now(), now())
ON CONFLICT (user_id) DO UPDATE SET
	token_ciphertext = EXCLUDED.token_ciphertext,
	refresh_token_ciphertext = EXCLUDED.refresh_token_ciphertext,
	scopes = EXCLUDED.scopes,
	expires_at = EXCLUDED.expires_at,
	provider_account_login = EXCLUDED.provider_account_login,
	revoked_at = NULL,
	last_validated_at = now(),
	updated_at = now(),
	version = github_oauth_tokens.version + 1;

-- name: GetGitHubConnectionReadiness :one
SELECT scopes, last_validated_at, token_ciphertext FROM github_oauth_tokens
WHERE user_id = $1 AND revoked_at IS NULL ORDER BY updated_at DESC LIMIT 1;

-- name: GetGitHubConfigRepoStatus :one
SELECT provider_account_id AS owner, split_part(display_name,'/',2)::text AS name, default_branch FROM control_config_repositories
WHERE owner_user_id = $1 AND provider='github' AND provisioned_at IS NOT NULL AND state='active'
ORDER BY provisioned_at DESC,id LIMIT 1;

-- name: GetGitHubScopes :one
SELECT scopes FROM github_oauth_tokens
WHERE user_id = $1 AND revoked_at IS NULL ORDER BY updated_at DESC LIMIT 1;

-- name: GetGitHubToken :one
SELECT token_ciphertext, provider_account_login, scopes, expires_at FROM github_oauth_tokens
WHERE user_id = $1 AND revoked_at IS NULL ORDER BY updated_at DESC LIMIT 1;

-- name: UpsertGitHubProvisioningAttempt :exec
INSERT INTO github_repo_provisioning_attempts (user_id, idempotency_key, state, repo_owner, repo_name, last_error, attempts, created_at, updated_at)
VALUES (sqlc.arg(user_id), sqlc.arg(idempotency_key), sqlc.arg(state), sqlc.arg(repo_owner), sqlc.arg(repo_name), sqlc.arg(last_error), 1, now(), now())
ON CONFLICT (user_id,idempotency_key) DO UPDATE SET
 state=EXCLUDED.state,repo_owner=EXCLUDED.repo_owner,repo_name=EXCLUDED.repo_name,
 last_error=EXCLUDED.last_error,attempts=github_repo_provisioning_attempts.attempts+1,updated_at=now();

-- name: UpsertGitHubConfigRepository :one
INSERT INTO control_config_repositories
 (id,owner_user_id,provider,external_ref,display_name,provider_account_id,external_repository_id,clone_url,publish_url,default_branch,authorization_ref,credential_capability,provisioned_at)
VALUES (sqlc.arg(id),sqlc.arg(user_id),'github',sqlc.arg(provider_repo_id),sqlc.arg(display_name),sqlc.arg(owner),sqlc.arg(provider_repo_id),sqlc.arg(clone_url),sqlc.arg(clone_url),sqlc.arg(default_branch),sqlc.arg(authorization_ref),'provider_user_repository',now())
ON CONFLICT (owner_user_id,provider,external_ref) DO UPDATE SET
 display_name=EXCLUDED.display_name,provider_account_id=EXCLUDED.provider_account_id,external_repository_id=EXCLUDED.external_repository_id,
 clone_url=EXCLUDED.clone_url,publish_url=EXCLUDED.publish_url,default_branch=EXCLUDED.default_branch,authorization_ref=EXCLUDED.authorization_ref,
 credential_capability=EXCLUDED.credential_capability,provisioned_at=now(),updated_at=now(),version=control_config_repositories.version+1
RETURNING id;

-- name: MarkGitHubProvisioningSucceeded :exec
UPDATE github_repo_provisioning_attempts SET state='succeeded',updated_at=now()
WHERE user_id=sqlc.arg(user_id) AND idempotency_key=sqlc.arg(idempotency_key);
