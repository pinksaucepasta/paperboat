-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Who chose an assignment's repository: the owner for this machine ('machine': pb config sync apply
-- --repository, the machine file, a recipe) or a workspace policy config_sync rule ('tag_rule'). A
-- rule-chosen assignment follows the rule on the next apply; a machine choice always wins.
ALTER TABLE paperboat.control_config_assignments ADD COLUMN source text NOT NULL DEFAULT 'machine';
ALTER TABLE paperboat.control_config_assignments ADD CONSTRAINT control_config_assignments_source_check
    CHECK (source IN ('machine', 'tag_rule')) NOT VALID;

-- +goose Down
ALTER TABLE paperboat.control_config_assignments DROP COLUMN source;
