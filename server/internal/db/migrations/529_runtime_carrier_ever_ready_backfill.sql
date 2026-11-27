-- +goose Up
-- Data only (528 added the column): a carrier that is ready now has been ready since its assignment, so a flap
-- right after deploy still keeps its edge in every-holder readiness.
UPDATE paperboat.runtime_carrier_attachments SET ever_ready=true WHERE ready AND NOT ever_ready;

-- +goose Down

