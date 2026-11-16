-- name: RecipeReachAllows :one
SELECT paperboat.recipe_reach_allows(sqlc.arg(recipe_id)::text, sqlc.arg(machine_id)::text, sqlc.arg(capability)::text, NULL::integer)::boolean AS allowed;
