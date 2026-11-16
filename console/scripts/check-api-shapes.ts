// Live conformance of the API validator (W-08): reads every documented GET operation that needs no path parameter through the
// live console, signed in, and checks each answer against lib/api-validate.ts. Prints PASS/FAIL per operation.
//   STATE=<playwright storage state> CONSOLE_ORIGIN=<console origin> bun scripts/check-api-shapes.ts
// Read-only. Nothing from an answer is printed except the field path that is wrong.
import { readFileSync } from "node:fs"
import { documentedOperations, problem, operationSchema } from "../lib/api-validate"

const { STATE, CONSOLE_ORIGIN: origin } = process.env
if (!STATE || !origin) throw new Error("STATE and CONSOLE_ORIGIN are required; see the header of check-api-shapes.ts")
const cookies = (JSON.parse(readFileSync(STATE, "utf8")).cookies as { name: string; value: string; domain: string }[])
  .filter((cookie) => origin.includes(cookie.domain.replace(/^\./, "")))
  .map((cookie) => `${cookie.name}=${cookie.value}`)
  .join("; ")

// Operations that take other query strings than the default.
const day = 86_400_000
const queries: Record<string, string> = {
  "/v1/selfhost/installations": "limit=5&scope_kind=account",
  "/v1/machines/summary": "workspace=personal",
  "/v1/machines/running-counts": "workspace=personal",
  "/v1/usage": `workspace=personal&start=${new Date(Date.now() - 7 * day).toISOString()}&end=${new Date().toISOString()}`,
}
const read = async (path: string) => {
  const response = await fetch(`${origin}/api${path}`, { headers: { cookie: cookies, accept: "application/json" } })
  return { status: response.status, body: (await response.json().catch(() => null)) as { data?: unknown } | null }
}

let failed = 0
let checked = 0
const firstIds = new Map<string, string>() // "/v1/machines" -> id of its first item, for the operations under it
const verdict = (key: string, data: unknown) => {
  const found = data === undefined ? "no data in the answer" : problem(operationSchema(key), data)
  checked++
  if (found) failed++
  console.log(`${found ? "FAIL" : "PASS"}  ${key}${found ? `  (${found})` : ""}`)
}
const get = documentedOperations().filter((k) => k.startsWith("GET "))
// {ws} is a workspace: the personal one.
for (const key of get.filter((k) => !k.includes("{") || /\{ws\}\/[a-z/]+$/.test(k))) {
  const path = key.slice(4).replace("{ws}", "personal")
  const { status, body } = await read(`${path}?${queries[path] ?? "workspace=personal&limit=5"}`)
  if (status !== 200) {
    console.log(`SKIP  ${key}  (HTTP ${status})`)
    continue
  }
  const id = (body?.data as { items?: { id?: string }[] } | undefined)?.items?.[0]?.id
  if (id) firstIds.set(path, id)
  verdict(key, body?.data)
}
// One item's own page, for the lists that had an item: GET /v1/machines/{machine_id} under the first machine.
for (const key of get.filter((k) => /^GET \/v1\/[a-z-]+\/\{[a-z_]+\}$/.test(k))) {
  const base = key.slice(4, key.indexOf("/{"))
  const id = firstIds.get(base)
  if (!id) continue
  const { status, body } = await read(`${base}/${encodeURIComponent(id)}?workspace=personal`)
  if (status !== 200) console.log(`SKIP  ${key}  (HTTP ${status})`)
  else verdict(key, body?.data)
}
console.log(`${checked - failed}/${checked} operations match the documented shape`)
process.exit(failed || !checked ? 1 : 0)
