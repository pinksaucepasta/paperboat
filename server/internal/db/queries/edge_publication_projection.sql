-- name: ListEdgeDNSPublicationResourcesV1 :many
-- Owners whose DNS records are still to be withdrawn (managed names are covered by wildcards; nothing is published).
SELECT owner_id AS id, max(resource_generation)::bigint AS generation FROM edge_dns_publications
WHERE (state<>'verified' OR desired_addresses<>'[]'::jsonb OR provider_records<>'[]'::jsonb)
 AND owner_id > sqlc.arg(after_id)::text
GROUP BY owner_id ORDER BY owner_id LIMIT sqlc.arg(batch_size);
