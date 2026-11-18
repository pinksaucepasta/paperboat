import type { Env } from "./types.ts"
import { catalog, unitKnown, type Reported } from "./store.ts"

const SKEW_MS = 120_000
const hex = (s: string) => Uint8Array.from(s.match(/../g) ?? [], (b) => parseInt(b, 16))

export async function sign(key: string, message: string): Promise<string> {
  const k = await crypto.subtle.importKey("raw", new TextEncoder().encode(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"])
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", k, new TextEncoder().encode(message)))
  return [...mac].map((b) => b.toString(16).padStart(2, "0")).join("")
}

export interface Parsed { vantage: string; probeTs: number; netOk: boolean; build: string; results: Reported }

/** A probe write is `HMAC-SHA256(vantage key, "<vantage>\n<timestamp>\n<body>")` in x-status-signature. Returns the parsed body or an error. */
export async function verifyProbe(env: Env, request: Request, body: string, now: number): Promise<Parsed | string> {
  let keys: Record<string, string>
  let data: { vantage?: unknown; ts?: unknown; net_ok?: unknown; build?: unknown; results?: unknown }
  try { keys = JSON.parse(env.PROBE_KEYS) ; data = JSON.parse(body) } catch { return "bad request" }
  const vantage = data.vantage
  if (typeof vantage !== "string" || !catalog.vantages.includes(vantage) || !keys[vantage]) return "unknown vantage"
  if (typeof data.ts !== "number" || Math.abs(now - data.ts) > SKEW_MS) return "stale timestamp"
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(keys[vantage]), { name: "HMAC", hash: "SHA-256" }, false, ["verify"])
  const sig = request.headers.get("x-status-signature") ?? ""
  if (!/^[0-9a-f]{64}$/.test(sig) || !(await crypto.subtle.verify("HMAC", key, hex(sig), new TextEncoder().encode(`${vantage}\n${data.ts}\n${body}`)))) return "bad signature"
  const results: Reported = {}
  if (data.net_ok === true && Array.isArray(data.results)) {
    for (const r of data.results.slice(0, 200)) {
      if (r && typeof r.unit === "string" && unitKnown(r.unit) && typeof r.ok === "boolean")
        results[r.unit] = [r.ok ? 1 : 0, Math.max(0, Math.round(Number(r.ms) || 0)), typeof r.error === "string" ? r.error.slice(0, 160) : undefined]
    }
  }
  return { vantage, probeTs: data.ts, netOk: data.net_ok === true, build: typeof data.build === "string" ? data.build : "", results }
}

const MAX_BODY = 65_536

/** At most MAX_BODY bytes of the body, as text: a check URL that streams forever cannot exhaust the Worker. */
async function bodyHead(res: Response): Promise<string> {
  const reader = res.body?.getReader()
  if (!reader) return ""
  const chunks: Uint8Array[] = []
  let size = 0
  while (size < MAX_BODY) {
    const { done, value } = await reader.read()
    if (done) break
    chunks.push(value)
    size += value.length
  }
  await reader.cancel().catch(() => undefined)
  return new TextDecoder().decode(await new Blob(chunks as BlobPart[]).arrayBuffer()).slice(0, MAX_BODY)
}

/** The cron Worker's own vantage point: plain HTTP fetches of the CHECKS targets, after its own internet check (rule 2). */
export async function cloudflareReport(env: Env): Promise<Parsed> {
  const netOk = await fetch(env.NET_CHECK_URL ?? "", { signal: AbortSignal.timeout(5000) }).then((r) => { void r.body?.cancel(); return r.ok }, () => false)
  const results: Reported = {}
  if (netOk) {
    let checks: { unit: string; url: string; expect?: string; redirect?: boolean }[] = []
    try { checks = JSON.parse(env.CHECKS ?? "[]") } catch { /* a malformed CHECKS secret means this vantage reports nothing; the other vantage points still decide */ }
    await Promise.all(checks.filter((c) => unitKnown(c.unit)).map(async (c) => {
      const start = Date.now()
      try {
        const res = await fetch(c.url, { signal: AbortSignal.timeout(10_000), redirect: "manual" })
        const text = c.expect ? await bodyHead(res) : ""
        const ok = (res.status >= 200 && res.status < (c.redirect ? 400 : 300)) && (!c.expect || text.includes(c.expect))
        results[c.unit] = [ok ? 1 : 0, Date.now() - start, ok ? undefined : `status ${res.status}`]
      } catch (e) {
        results[c.unit] = [0, Date.now() - start, e instanceof Error ? e.name : "error"]
      }
    }))
  }
  return { vantage: "cloudflare", probeTs: Date.now(), netOk, build: "worker", results }
}
