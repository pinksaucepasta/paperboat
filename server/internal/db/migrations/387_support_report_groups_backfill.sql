-- +goose Up
-- Data only: the still-live support_reports rows become occurrences and groups, one day of rows per statement
-- (rows live 7 days). Each statement adds a group's count only for occurrences it newly inserted, so a rerun
-- counts nothing twice.
-- +goose StatementBegin
DO $$
DECLARE d integer;
BEGIN
  FOR d IN REVERSE 7..0 LOOP
    WITH moved AS (
      INSERT INTO paperboat.support_report_occurrences
        (id, owner_id, support_reference, fingerprint, request_id, correlation_id, release, created_at, expires_at, sentry_event_id, reporting)
      SELECT id, owner_id, support_reference,
             paperboat.support_fingerprint(component, operation, stage, code, cause, error_type, http_status, outcome, source_file, source_function, source_line),
             request_id, correlation_id, release, created_at, expires_at, sentry_event_id, reporting
      FROM paperboat.support_reports
      WHERE expires_at > now() AND created_at >= now() - make_interval(days => d + 1) AND created_at < now() - make_interval(days => d)
      ON CONFLICT (id) DO NOTHING
      RETURNING id, fingerprint, created_at, expires_at
    )
    INSERT INTO paperboat.support_report_groups
      (owner_id, fingerprint, component, operation, stage, code, cause, error_type, http_status, outcome, source_file,
       source_function, source_line, severity, errno, error_chain, first_seen, last_seen, occurrences, expires_at)
    SELECT DISTINCT ON (r.owner_id, m.fingerprint)
      r.owner_id, m.fingerprint, r.component, r.operation, r.stage, r.code, r.cause, r.error_type, r.http_status, r.outcome,
      r.source_file, r.source_function, r.source_line, r.severity, r.errno, r.error_chain,
      min(m.created_at) OVER w, max(m.created_at) OVER w, count(*) OVER w, max(m.expires_at) OVER w
    FROM moved m JOIN paperboat.support_reports r USING (id)
    WINDOW w AS (PARTITION BY r.owner_id, m.fingerprint)
    ORDER BY r.owner_id, m.fingerprint, m.created_at DESC
    ON CONFLICT (owner_id, fingerprint) DO UPDATE SET
      first_seen = LEAST(support_report_groups.first_seen, EXCLUDED.first_seen),
      last_seen = GREATEST(support_report_groups.last_seen, EXCLUDED.last_seen),
      occurrences = support_report_groups.occurrences + EXCLUDED.occurrences,
      expires_at = GREATEST(support_report_groups.expires_at, EXCLUDED.expires_at);
  END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'support report backfill is forward-only'; END $$;
-- +goose StatementEnd
