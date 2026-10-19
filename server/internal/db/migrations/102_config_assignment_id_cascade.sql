-- +goose Up
-- Re-applying a changed config rewrites the assignment's id (SetControlConfigAssignment). A compare
-- admission pointing at the old id made that update fail with a foreign-key error, so the machine
-- could never be assigned again. Admissions follow the assignment's id; their version and conflict
-- revision still gate use.
ALTER TABLE paperboat.config_compare_admissions
    DROP CONSTRAINT config_compare_admissions_assignment_id_fkey,
    ADD CONSTRAINT config_compare_admissions_assignment_id_fkey
        FOREIGN KEY (assignment_id) REFERENCES paperboat.control_config_assignments(id) ON DELETE CASCADE ON UPDATE CASCADE;

-- +goose Down
ALTER TABLE paperboat.config_compare_admissions
    DROP CONSTRAINT config_compare_admissions_assignment_id_fkey,
    ADD CONSTRAINT config_compare_admissions_assignment_id_fkey
        FOREIGN KEY (assignment_id) REFERENCES paperboat.control_config_assignments(id) ON DELETE CASCADE;
