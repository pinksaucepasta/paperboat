-- name: UpsertWorkOSUser :one
WITH upsert_user AS (
	INSERT INTO users (id, workos_subject, primary_email, canonical_email, email_verified, display_name, status, created_at, updated_at)
	VALUES (sqlc.arg(user_id), sqlc.arg(workos_subject), sqlc.arg(primary_email), sqlc.narg(canonical_email), sqlc.arg(email_verified), sqlc.arg(display_name), 'active', now(), now())
	ON CONFLICT (workos_subject) DO UPDATE SET canonical_email = CASE WHEN users.canonical_email IS NULL THEN NULL ELSE paperboat.canonical_email_address(EXCLUDED.primary_email) END, primary_email = EXCLUDED.primary_email, email_verified = EXCLUDED.email_verified, display_name = EXCLUDED.display_name, updated_at = now(), version = users.version + 1
	RETURNING id, workos_subject, primary_email, email_verified, display_name, status, role, created_at
), identity AS (
	INSERT INTO user_identities (user_id, provider, provider_subject, email, created_at, updated_at)
	SELECT id, 'workos', workos_subject, primary_email, now(), now() FROM upsert_user
	ON CONFLICT (provider, provider_subject) DO UPDATE SET email = EXCLUDED.email, updated_at = now()
)
SELECT id, workos_subject, primary_email, email_verified, display_name, status, role, created_at FROM upsert_user;

-- name: CreateBrowserSession :exec
INSERT INTO sessions (id, user_id, session_hash, csrf_hash, expires_at, workos_session_id, workos_refresh_token_ciphertext, workos_access_expires_at, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(session_hash), sqlc.arg(csrf_hash), sqlc.arg(expires_at), sqlc.narg(workos_session_id), sqlc.narg(workos_refresh_token_ciphertext), sqlc.narg(workos_access_expires_at), now(), now());

-- name: AuthenticateBrowserSession :one
SELECT s.id AS session_id, s.user_id, s.expires_at, s.csrf_hash, s.operator_authenticated_at, s.workos_session_id, s.workos_access_expires_at,
       u.id, u.workos_subject, u.primary_email, u.email_verified, u.display_name, u.status, u.role, u.created_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.session_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now() AND u.status = 'active';

-- name: RotateBrowserSession :one
UPDATE sessions SET session_hash = sqlc.arg(session_hash), csrf_hash = sqlc.arg(csrf_hash), expires_at = sqlc.arg(expires_at),
	rotated_at = now(), updated_at = now(), version = version + 1
WHERE id = sqlc.arg(id) AND revoked_at IS NULL AND expires_at > now()
RETURNING operator_authenticated_at;

-- name: RefreshBrowserSessionCSRF :execrows
UPDATE sessions SET csrf_hash = sqlc.arg(csrf_hash), updated_at = now()
WHERE id = sqlc.arg(id) AND revoked_at IS NULL AND expires_at > now();

-- name: RevokeBrowserSession :one
UPDATE sessions SET revoked_at = now(), workos_refresh_token_ciphertext=NULL, updated_at = now(), version = version + 1
WHERE session_hash = $1 AND revoked_at IS NULL RETURNING user_id, workos_session_id;

-- name: BrowserSessionCSRFExists :one
SELECT EXISTS (SELECT 1 FROM sessions WHERE session_hash = $1 AND csrf_hash = $2 AND revoked_at IS NULL AND expires_at > now());


-- name: UserHasActiveSubscription :one
SELECT EXISTS (SELECT 1 FROM user_machine_entitlements WHERE user_id=$1 AND state IN ('active','trialing') AND current_period_end>now());
