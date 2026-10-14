-- +goose Up
-- Endpoint transport trust belongs to the endpoint certificate. ENV signing
-- keys remain in account_e2ee_keys and are never implied by enrollment.
ALTER TABLE paperboat.peer_endpoint_certificates
  ADD COLUMN signer_public_key bytea;

UPDATE paperboat.peer_endpoint_certificates AS certificate
SET signer_public_key = account_key.public_key
FROM paperboat.account_e2ee_keys AS account_key
WHERE certificate.key_id = account_key.key_id;

ALTER TABLE paperboat.peer_endpoint_certificates
  ALTER COLUMN signer_public_key SET NOT NULL,
  ADD CONSTRAINT peer_endpoint_certificates_signer_public_key_check
    CHECK (octet_length(signer_public_key) = 32);

ALTER TABLE paperboat.peer_endpoint_certificates
  DROP CONSTRAINT peer_endpoint_certificates_key_id_fkey,
  DROP CONSTRAINT peer_endpoint_certificates_user_id_fkey;

ALTER TABLE paperboat.peer_endpoint_certificate_operations
  DROP CONSTRAINT peer_endpoint_certificate_operations_user_id_fkey;

ALTER TABLE paperboat.peer_endpoint_certificate_revocations
  DROP CONSTRAINT peer_endpoint_certificate_revocations_user_id_fkey;

ALTER TABLE paperboat.peer_endpoint_certificates
  ADD CONSTRAINT peer_endpoint_certificates_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;

ALTER TABLE paperboat.peer_endpoint_certificate_operations
  ADD CONSTRAINT peer_endpoint_certificate_operations_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;

ALTER TABLE paperboat.peer_endpoint_certificate_revocations
  ADD CONSTRAINT peer_endpoint_certificate_revocations_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES paperboat.users(id) ON DELETE CASCADE;
