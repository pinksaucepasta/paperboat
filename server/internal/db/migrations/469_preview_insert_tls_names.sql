-- +goose Up
SET lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);

-- A new preview adds only its own terminated TLS name. The shared names lock
-- still orders claims, but inserts need not read other previews' names.
-- +goose StatementBegin
CREATE FUNCTION paperboat.check_inserted_preview_443_names() RETURNS trigger
LANGUAGE plpgsql SET search_path TO paperboat, pg_temp AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM inserted_previews p
    CROSS JOIN LATERAL (SELECT lower(rtrim(substring(p.endpoint FROM 9), '.')) AS hostname) n
    JOIN shared_443_names_v1 a ON a.passthrough
      AND (a.hostname = n.hostname OR
        (n.hostname LIKE '*.%' AND substring(a.hostname FROM position('.' IN a.hostname) + 1) = substring(n.hostname FROM 3)))
    WHERE p.terminal_state = 'active'
  ) THEN
    RAISE EXCEPTION 'hostname is already published in the other TLS mode on port 443; delete the conflicting route or domain, or choose another hostname'
      USING ERRCODE='23505',CONSTRAINT='shared_443_hostname_mode_conflict';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

DROP TRIGGER shared_443_check ON paperboat.preview_leases;
CREATE TRIGGER shared_443_check
AFTER DELETE OR UPDATE OF endpoint, terminal_state ON paperboat.preview_leases
FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();
CREATE TRIGGER shared_443_check_insert
AFTER INSERT ON paperboat.preview_leases REFERENCING NEW TABLE AS inserted_previews
FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_inserted_preview_443_names();

-- +goose Down
SET lock_timeout = '5s';
SELECT pg_advisory_xact_lock(1885686843);
DROP TRIGGER shared_443_check_insert ON paperboat.preview_leases;
DROP TRIGGER shared_443_check ON paperboat.preview_leases;
CREATE TRIGGER shared_443_check
AFTER INSERT OR DELETE OR UPDATE OF endpoint, terminal_state ON paperboat.preview_leases
FOR EACH STATEMENT EXECUTE FUNCTION paperboat.check_shared_443_names_v1();
DROP FUNCTION paperboat.check_inserted_preview_443_names();
