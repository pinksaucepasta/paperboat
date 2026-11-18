-- +goose Up
SET LOCAL lock_timeout = '5s';
-- An operator's ban of an account or a team: why (the operator's reference), who, when, and, for an account, the identity that
-- may not sign in or sign up again (the WorkOS subject and the canonical e-mail, which already ignores case and +suffixes).
-- The enforcement state stays users.status / teams.suspended; this is the reason shown to operators and the identity gate.
-- A row is never deleted: lifting a ban sets lifted_at, so the history stays. cause_ban_id links a team ban made because
-- its owner was banned, so lifting the owner's ban lifts it too.
CREATE TABLE paperboat.bans (
    id text PRIMARY KEY,
    subject_kind text NOT NULL CHECK (subject_kind IN ('user','team')),
    subject_id text NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 128),
    banned_by text NOT NULL,
    banned_at timestamptz NOT NULL DEFAULT now(),
    canonical_email text,
    workos_subject text,
    subscriptions text NOT NULL DEFAULT 'keep' CHECK (subscriptions IN ('keep','period_end','immediate')),
    cause_ban_id text,
    lifted_at timestamptz,
    lifted_by text,
    CHECK ((lifted_at IS NULL) = (lifted_by IS NULL))
);
CREATE UNIQUE INDEX bans_subject_active ON paperboat.bans (subject_kind, subject_id) WHERE lifted_at IS NULL;
CREATE INDEX bans_email_active ON paperboat.bans (canonical_email) WHERE lifted_at IS NULL AND canonical_email IS NOT NULL;
CREATE INDEX bans_subject_active_workos ON paperboat.bans (workos_subject) WHERE lifted_at IS NULL AND workos_subject IS NOT NULL;
CREATE INDEX bans_cause ON paperboat.bans (cause_ban_id) WHERE cause_ban_id IS NOT NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE paperboat.bans;
