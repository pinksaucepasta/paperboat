-- name: APITokenScopeAllows :one
SELECT paperboat.api_token_scope_allows(sqlc.arg(token_id)::text, sqlc.arg(machine_id)::text, sqlc.arg(capability)::text, NULL::integer)::boolean AS allowed;
