import "server-only"

/**
 * The one server-side interface the panel's Status section uses for the public status page. Today the page is ours (status/, a
 * Cloudflare Pages Functions API); a hosted provider (incident.io, Atlassian Statuspage) means a different implementation of
 * this type and nothing else in the panel changes.
 */
export type StatusProvider = {
  /** False when the panel has no status page connected: the section says so and never errors. */
  configured(): boolean
  state(): Promise<unknown>
  createIncident(input: unknown): Promise<unknown>
  updateIncident(id: string, input: unknown): Promise<unknown>
  createMaintenance(input: unknown): Promise<unknown>
  cancelMaintenance(id: string): Promise<unknown>
}

export class StatusProviderError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

/** Our own status API: STATUS_API_URL (the page's origin) with the admin bearer token STATUS_ADMIN_TOKEN. */
export const ownStatusProvider: StatusProvider = (() => {
  const base = () => process.env.STATUS_API_URL
  const token = () => process.env.STATUS_ADMIN_TOKEN
  async function call(path: string, method: "GET" | "POST", body?: unknown) {
    let res: Response
    try {
      res = await fetch(new URL(`/api/admin/${path}`, base()), {
        method,
        headers: { authorization: `Bearer ${token()}`, "content-type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
        cache: "no-store",
        signal: AbortSignal.timeout(15_000),
      })
    } catch {
      throw new StatusProviderError(502, "The status service did not answer.")
    }
    const parsed = (await res.json().catch(() => ({}))) as { error?: string }
    if (!res.ok) throw new StatusProviderError(res.status === 401 ? 502 : res.status, parsed.error ?? `The status service answered ${res.status}.`)
    return parsed
  }
  return {
    configured: () => Boolean(base() && token()),
    state: () => call("state", "GET"),
    createIncident: (input) => call("incidents", "POST", input),
    updateIncident: (id, input) => call(`incidents/${encodeURIComponent(id)}/updates`, "POST", input),
    createMaintenance: (input) => call("maintenance", "POST", input),
    cancelMaintenance: (id) => call(`maintenance/${encodeURIComponent(id)}/cancel`, "POST", {}),
  }
})()

export const statusProvider: StatusProvider = ownStatusProvider
