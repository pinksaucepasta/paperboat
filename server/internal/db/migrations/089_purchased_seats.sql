-- +goose Up
ALTER TABLE paperboat.billing_workspaces
 ADD COLUMN pending_seat_capacity integer CHECK (pending_seat_capacity > 0),
 ADD COLUMN scheduled_quantity integer CHECK (scheduled_quantity > 0),
 ADD COLUMN scheduled_quantity_at timestamptz;
-- +goose Down
ALTER TABLE paperboat.billing_workspaces DROP COLUMN pending_seat_capacity, DROP COLUMN scheduled_quantity, DROP COLUMN scheduled_quantity_at;
