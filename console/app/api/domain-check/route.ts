import { randomBytes } from "node:crypto"
import { Resolver } from "node:dns/promises"
import type { DomainCheck } from "@/lib/machine-domains"
import { controlPlaneFetch } from "@/lib/server/control-plane"

/**
 * Checks that a machine domain is set up: `*.domain` must resolve to Paperboat's local gateway.
 * A random label proves the record is a wildcard, not one name. Only signed-in people may ask,
 * so this is not an open DNS lookup service.
 */
const gateway = "127.100.0.1"
const domainPattern = /^(?=.{1,253}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/

export async function GET(request: Request): Promise<Response> {
  const domain = new URL(request.url).searchParams.get("domain")?.toLowerCase() ?? ""
  if (!domainPattern.test(domain)) return problem(400, "invalid_domain", "Enter a domain like example.com.")

  const signedIn = await controlPlaneFetch("/v1/me", {
    headers: { accept: "application/json", cookie: request.headers.get("cookie") ?? "" },
    signal: AbortSignal.timeout(5_000),
  }).catch(() => null)
  if (!signedIn) return problem(503, "unavailable", "Paperboat is temporarily unavailable.")
  if (signedIn.status === 401) return problem(401, "unauthenticated", "Authentication is required.")
  if (!signedIn.ok) return problem(503, "unavailable", "Paperboat is temporarily unavailable.")

  const resolver = new Resolver({ timeout: 3_000, tries: 2 })
  const probe = `pb-check-${randomBytes(6).toString("hex")}.${domain}`
  const [lookup, zone] = await Promise.all([
    resolver.resolve4(probe).then(
      (addresses) => ({ addresses }),
      (error: NodeJS.ErrnoException) => ({ code: error.code }),
    ),
    findZone(resolver, domain),
  ])
  let result: DomainCheck
  if ("addresses" in lookup) {
    const ready = lookup.addresses.length === 1 && lookup.addresses[0] === gateway
    result = ready ? { status: "ready", zone } : { status: "wrong_address", addresses: lookup.addresses, zone }
  } else {
    result =
      lookup.code === "ENOTFOUND" || lookup.code === "ENODATA"
        ? { status: "not_found", zone }
        : { status: "unavailable", zone }
  }
  return Response.json({ data: result }, { headers: { "cache-control": "no-store" } })
}

/**
 * The DNS zone the record goes in (the domain managed at the DNS provider), found by walking up
 * from the domain to the first name with an SOA record. It decides the record's Name: `*` at the
 * zone itself, `*.home` for `home.example.net` in the `example.net` zone.
 */
async function findZone(resolver: Resolver, domain: string): Promise<string | undefined> {
  const labels = domain.split(".")
  for (let index = 0; index < labels.length - 1; index++) {
    const candidate = labels.slice(index).join(".")
    try {
      await resolver.resolveSoa(candidate)
      return candidate
    } catch {
      // Not a zone apex (or no answer); try the parent.
    }
  }
}

function problem(status: number, code: string, message: string): Response {
  return Response.json({ error: { code, message } }, { status, headers: { "cache-control": "no-store" } })
}
