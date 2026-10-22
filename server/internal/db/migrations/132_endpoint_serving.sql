-- +goose Up
-- The edge process that last pulled its runtime snapshot advertising the endpoints capability.
-- An agent uses the endpoint engine for previews only while every edge of its attachment serves
-- endpoints, so a rollout never strands a preview on an edge that cannot admit its route.
ALTER TABLE paperboat.control_tunnel_nodes ADD COLUMN endpoints_epoch text;

-- PLAN 3a step 2: one read model for "where is this preview served". Until the per-preview carrier
-- path is deleted (step 5) a preview is served either by its own carrier attachment or, when its
-- agent serves endpoints, by an endpoint route on the agent's runtime carrier. Every reader of
-- preview serving state reads this view; only the old attachment writers touch the table.
-- An endpoint route appears only on an edge that observed it within 30 s over a valid runtime
-- carrier, so DNS and browser access never name an edge that lacks the route. An endpoint route has
-- no lease generation of its own (renewal never fences it), so it reports 1.
-- +goose StatementBegin
CREATE VIEW paperboat.preview_routes AS
SELECT a.preview_id, a.account_id, a.operation_id, a.owner_machine_id, a.owner_session_id, a.host_id,
 a.tunnel_id, a.connector_id, a.connector_session_id, a.process_generation, a.config_generation,
 a.route_id, a.route_generation, a.lease_generation, a.config_content_hash, a.edge_node_id,
 a.edge_process_epoch, a.attachment_generation, a.machine_identity_public_key, a.edge_endpoints,
 a.edge_carrier_server_spki_sha256, a.edge_carrier_server_certificate_chain_pem, a.state, a.edge_ready,
 a.origin_ready, a.expires_at, false AS endpoint
FROM paperboat.preview_lease_carrier_attachments a
UNION ALL
SELECT p.id, p.account_id, x.route_id, p.owner_machine_id, p.owner_session_id, p.owner_machine_id,
 ra.admission->'binding'->>'tunnel_id', ra.admission->'binding'->>'connector_id', ra.session_id,
 ra.worker_generation, (ra.admission->'binding'->>'config_generation')::bigint,
 x.route_id, r.revision, 1::bigint, ra.admission->>'config_content_hash', x.edge_node_id,
 x.edge_process_epoch, x.generation, ra.public_identity_key,
 ARRAY(SELECT jsonb_array_elements_text(ra.admission->'edge_endpoints')),
 ra.admission->'binding'->>'edge_carrier_server_spki_sha256',
 ra.admission->'binding'->>'edge_carrier_server_certificate_chain_pem',
 CASE WHEN p.origin_state='ready' THEN 'ready' ELSE 'edge_ready' END, true, p.origin_state='ready',
 ra.expires_at, true
FROM paperboat.endpoint_edge_assignments x
JOIN paperboat.endpoint_routes r ON r.id=x.route_id
JOIN paperboat.endpoints e ON e.id=x.endpoint_id AND e.kind='preview' AND e.desired_state='active'
JOIN paperboat.preview_leases p ON p.id=e.preview_lease_id
JOIN paperboat.runtime_carrier_attachments ra ON ra.machine_id=e.agent_machine_id AND ra.edge_node_id=x.edge_node_id
 AND ra.edge_process_epoch=x.edge_process_epoch AND ra.session_id=x.carrier_session_id
WHERE x.desired='active' AND x.observed_ready AND x.observed_at>now()-interval '30 seconds'
 AND paperboat.runtime_carrier_valid(ra.route_id,ra.edge_node_id,ra.edge_process_epoch,false);
-- +goose StatementEnd

-- +goose Down
DROP VIEW paperboat.preview_routes;
ALTER TABLE paperboat.control_tunnel_nodes DROP COLUMN endpoints_epoch;
