-- +goose Up
-- An edge advertises the port its public HTTPS listener is published on; the server dispatches to it (NULL: 443).
SET LOCAL lock_timeout = '5s';
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN public_https_port integer;
ALTER TABLE paperboat.control_tunnel_nodes ADD CONSTRAINT control_tunnel_nodes_public_https_port_check CHECK (public_https_port IS NULL OR public_https_port BETWEEN 1 AND 65535) NOT VALID;
