-- +goose Up
-- small-table: operator announcements (a handful). System notices about our global nodes go to operators, shown in
-- the admin bell: a new audience, 'operators', with no audience value.
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.announcements DROP CONSTRAINT announcements_audience_kind_check;
-- The value check of 420 references two columns, so PostgreSQL named it, not after the column: find it by its text.
-- +goose StatementBegin
DO $$
DECLARE name text;
BEGIN
    SELECT conname INTO STRICT name FROM pg_constraint
     WHERE conrelid = 'paperboat.announcements'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%audience_value%';
    EXECUTE format('ALTER TABLE paperboat.announcements DROP CONSTRAINT %I', name);
END $$;
-- +goose StatementEnd
ALTER TABLE paperboat.announcements ADD CONSTRAINT announcements_audience_kind_check
    CHECK (audience_kind IN ('all', 'plan', 'team', 'user', 'operators')) NOT VALID;
ALTER TABLE paperboat.announcements ADD CONSTRAINT announcements_audience_value_check
    CHECK (length(audience_value) <= 128 AND ((audience_kind IN ('all', 'operators')) = (audience_value = ''))) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DELETE FROM paperboat.announcements WHERE audience_kind = 'operators';
ALTER TABLE paperboat.announcements DROP CONSTRAINT announcements_audience_kind_check;
ALTER TABLE paperboat.announcements DROP CONSTRAINT announcements_audience_value_check;
ALTER TABLE paperboat.announcements ADD CONSTRAINT announcements_audience_kind_check CHECK (audience_kind IN ('all', 'plan', 'team', 'user'));
ALTER TABLE paperboat.announcements ADD CONSTRAINT announcements_audience_value_check CHECK (length(audience_value) <= 128 AND ((audience_kind = 'all') = (audience_value = '')));
