import type { APIRoute, GetStaticPaths } from "astro"
import configSync from "../../../../cli/internal/hostruntime/configsync/schema.json"
import policy from "../../../../shared/policy/schema.json"
import recipe from "../../../../shared/recipe/schema.json"

// The schemas are generated from the Go types (see shared/schemagen) and served as they are, with the
// address they are served at as their $id.
const schemas: Record<string, object> = { policy, recipe, "config-sync": configSync }

export const getStaticPaths = (() => Object.keys(schemas).map((name) => ({ params: { name } }))) satisfies GetStaticPaths

export const GET: APIRoute = ({ params, site }) => {
  const name = params.name ?? ""
  const id = new URL(`/schemas/${name}.json`, site).href
  return new Response(JSON.stringify({ $id: id, ...schemas[name] }, null, 2) + "\n", {
    headers: { "Content-Type": "application/schema+json" },
  })
}
