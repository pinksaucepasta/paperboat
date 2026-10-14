-- +goose Up
ALTER TABLE paperboat.control_config_repositories ADD COLUMN provisioned_at timestamptz;

INSERT INTO paperboat.control_config_repositories
 (id,owner_user_id,provider,external_ref,display_name,provider_account_id,external_repository_id,clone_url,publish_url,default_branch,authorization_ref,credential_capability,provisioned_at)
SELECT id,user_id,'github',provider_repo_id,owner||'/'||name,owner,provider_repo_id,clone_url,clone_url,default_branch,'github-user:'||user_id,'provider_user_repository',provisioned_at
FROM paperboat.github_config_repositories WHERE provider_repo_id<>'' AND provisioned_at IS NOT NULL
ON CONFLICT (owner_user_id,provider,external_ref) DO UPDATE SET provisioned_at=EXCLUDED.provisioned_at;
DROP TABLE paperboat.github_config_repositories;

UPDATE paperboat.control_config_repositories SET external_repository_id=NULL,authorization_ref=NULL WHERE provider='git';

ALTER TABLE paperboat.user_identities DROP COLUMN id;
ALTER TABLE paperboat.user_identities DROP CONSTRAINT user_identities_provider_provider_subject_key;
ALTER TABLE paperboat.user_identities ADD PRIMARY KEY(provider,provider_subject);

ALTER TABLE paperboat.github_oauth_tokens DROP COLUMN id;
ALTER TABLE paperboat.github_oauth_tokens ADD PRIMARY KEY USING INDEX idx_github_oauth_tokens_user_unique;

ALTER TABLE paperboat.github_repo_provisioning_attempts DROP COLUMN id;
ALTER TABLE paperboat.github_repo_provisioning_attempts DROP CONSTRAINT github_repo_provisioning_attempts_idempotency_key_key;
ALTER TABLE paperboat.github_repo_provisioning_attempts ADD PRIMARY KEY(user_id,idempotency_key);

ALTER TABLE paperboat.operator_actions DROP COLUMN id;
ALTER TABLE paperboat.operator_actions DROP CONSTRAINT operator_actions_actor_user_id_idempotency_key_key;
ALTER TABLE paperboat.operator_actions ADD PRIMARY KEY(actor_user_id,idempotency_key);
