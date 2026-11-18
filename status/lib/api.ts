import { esc, mailEnabled, sendConfirmation, sendIncident } from "./mail.ts"
import { verifyProbe } from "./probe.ts"
import { feed } from "./rss.ts"
import { fanout, subscribeWebhook, unsubscribeWebhook } from "./webhook.ts"
import { FRESH_MS } from "./decide.ts"
import { admitProbe, buildSnapshot, catalog, readSnapshot, saveReport } from "./store.ts"
import { INCIDENT_STATUSES, type Env, type Incident } from "./types.ts"

export type Ctx = { waitUntil(promise: Promise<unknown>): void }
const json = (body: unknown, status = 200, cache = "no-store") =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": cache } })
const fail = (status: number, error: string) => json({ error }, status)
const button = (label: string, token: string, action: string) => new Response(`<!doctype html><meta charset=utf-8><title>Paperboat status</title><body style="font:16px system-ui;padding:2rem"><form method="post" action="${esc(action)}"><input type="hidden" name="t" value="${esc(token)}"><button style="font:inherit;padding:.6rem 1rem">${esc(label)}</button></form>`, { headers: { "content-type": "text/html; charset=utf-8" } })
const page = (text: string) => new Response(`<!doctype html><meta charset=utf-8><title>Paperboat status</title><body style="font:16px system-ui;padding:2rem"><p>${esc(text)}</p>`, { headers: { "content-type": "text/html; charset=utf-8" } })

// Both sides are SHA-256 digests, so the comparison is over equal lengths and does not stop at the first difference.
const same = (a: Uint8Array, b: Uint8Array) => a.reduce((diff, x, i) => diff | (x ^ b[i]), 0) === 0

async function adminAuthorized(env: Env, request: Request) {
  const given = (request.headers.get("authorization") ?? "").replace(/^Bearer /, "")
  const [a, b] = await Promise.all([given, env.ADMIN_TOKEN].map((s) => crypto.subtle.digest("SHA-256", new TextEncoder().encode(s))))
  return same(new Uint8Array(a), new Uint8Array(b))
}

async function incidentRow(db: D1Database, id: string): Promise<Incident | null> {
  const row = await db.prepare("SELECT * FROM incidents WHERE id = ?").bind(id).first<Omit<Incident, "components" | "updates"> & { components: string }>()
  if (!row) return null
  const updates = (await db.prepare("SELECT status, body, ts FROM incident_updates WHERE incident_id = ? ORDER BY ts DESC").bind(id).all<Incident["updates"][number]>()).results
  return { ...row, components: JSON.parse(row.components), updates }
}

const componentIds = (v: unknown): string[] | null =>
  Array.isArray(v) && v.every((x) => typeof x === "string" && catalog.components.some((c) => c.id === x)) ? (v as string[]) : null
const text = (v: unknown, max: number) => (typeof v === "string" && v.trim().length > 0 && v.length <= max ? v.trim() : null)

const MAIL_LIMIT = 500
/** Mail the confirmed subscribers and report what happened: the admin is told when mail failed or the list was cut off. */
async function notify(env: Env, incident: Incident, update: { status: string; body: string }) {
  if (!mailEnabled(env)) return { sent: 0, failed: 0, truncated: false }
  const subs = (await env.DB.prepare("SELECT email, token FROM subscribers WHERE confirmed = 1 ORDER BY created LIMIT ?").bind(MAIL_LIMIT + 1).all<{ email: string; token: string }>()).results
  const batch = subs.slice(0, MAIL_LIMIT)
  const failed = await sendIncident(env, batch, incident, update, `inc-${incident.id}-${update.status}-${incident.updated}`)
  return { sent: batch.length - failed, failed, truncated: subs.length > MAIL_LIMIT }
}

const IDEM = /^[\w-]{8,64}$/
/** A retried admin write (same Idempotency-Key) returns the first answer instead of creating a second incident and mailing twice. */
async function once(db: D1Database, request: Request, now: number, run: () => Promise<Response>): Promise<Response> {
  const key = request.headers.get("idempotency-key")
  if (!key) return run()
  if (!IDEM.test(key)) return fail(400, "bad idempotency key")
  const seen = await db.prepare("SELECT v FROM kv WHERE k = ?").bind(`idem:${key}`).first<{ v: string }>()
  if (seen) return json({ ...JSON.parse(seen.v).body, replayed: true })
  const res = await run()
  if (res.ok) await db.prepare("INSERT OR IGNORE INTO kv (k, v) VALUES (?, ?)").bind(`idem:${key}`, JSON.stringify({ ts: now, body: await res.clone().json() })).run()
  return res
}

async function admin(request: Request, env: Env, path: string, now: number): Promise<Response> {
  if (!env.ADMIN_TOKEN) return fail(503, "the admin API is not configured") // closed, never open by default
  if (!(await adminAuthorized(env, request))) return fail(401, "unauthorized")
  const db = env.DB
  if (request.method === "GET" && path === "/api/admin/state") return json((await readSnapshot(db)) ?? (await buildSnapshot(db, now)))
  if (request.method !== "POST") return fail(405, "method not allowed")
  const body = (await request.json().catch(() => null)) as Record<string, unknown> | null
  if (!body) return fail(400, "bad json")
  if (path === "/api/admin/incidents") {
    const title = text(body.title, 140), message = text(body.body, 4000), components = componentIds(body.components)
    const status = String(body.status ?? "investigating")
    if (!title || /[\r\n]/.test(title) || !message || !components || !(INCIDENT_STATUSES as readonly string[]).includes(status)) return fail(400, "a one-line title, body, components and a valid status are required")
    return once(db, request, now, async () => {
    const id = crypto.randomUUID()
    await db.batch([
      db.prepare("INSERT INTO incidents (id, title, status, components, created, updated, resolved) VALUES (?, ?, ?, ?, ?, ?, ?)").bind(id, title, status, JSON.stringify(components), now, now, status === "resolved" ? now : null),
      db.prepare("INSERT INTO incident_updates (id, incident_id, status, body, ts) VALUES (?, ?, ?, ?, ?)").bind(crypto.randomUUID(), id, status, message, now),
    ])
    await buildSnapshot(db, now)
    const incident = (await incidentRow(db, id))!
    return json({ ...incident, mail: await notify(env, incident, { status, body: message }), webhooks: await fanout(env, { type: `incident.${status === "resolved" ? "resolved" : "created"}`, data: incident }, now) }, 201)
    })
  }
  const update = path.match(/^\/api\/admin\/incidents\/([\w-]+)\/updates$/)
  if (update) {
    const incident = await incidentRow(db, update[1])
    const message = text(body.body, 4000), status = String(body.status ?? "")
    if (!incident) return fail(404, "no such incident")
    if (incident.resolved !== null) return fail(409, "the incident is resolved")
    if (!message || !(INCIDENT_STATUSES as readonly string[]).includes(status)) return fail(400, "body and a valid status are required")
    return once(db, request, now, async () => {
    await db.batch([
      db.prepare("INSERT INTO incident_updates (id, incident_id, status, body, ts) VALUES (?, ?, ?, ?, ?)").bind(crypto.randomUUID(), incident.id, status, message, now),
      db.prepare("UPDATE incidents SET status = ?, updated = ?, resolved = ? WHERE id = ?").bind(status, now, status === "resolved" ? now : null, incident.id),
    ])
    await buildSnapshot(db, now)
    const next = (await incidentRow(db, incident.id))!
    return json({ ...next, mail: await notify(env, next, { status, body: message }), webhooks: await fanout(env, { type: `incident.${status === "resolved" ? "resolved" : "updated"}`, data: next }, now) })
    })
  }
  if (path === "/api/admin/maintenance") {
    const title = text(body.title, 140), components = componentIds(body.components)
    const starts = Number(body.starts), ends = Number(body.ends)
    if (!title || !components || !Number.isFinite(starts) || !Number.isFinite(ends) || ends <= starts || ends - starts > 7 * 86_400_000) return fail(400, "title, components and a window of at most 7 days are required")
    const id = crypto.randomUUID(), message = typeof body.body === "string" ? body.body.slice(0, 2000) : ""
    await db.prepare("INSERT INTO maintenance (id, title, body, components, starts, ends, created) VALUES (?, ?, ?, ?, ?, ?, ?)").bind(id, title, message, JSON.stringify(components), starts, ends, now).run()
    await buildSnapshot(db, now)
    return json({ id, webhooks: await fanout(env, { type: "maintenance.created", data: { id, title, body: message, components, starts, ends } }, now) }, 201)
  }
  const cancel = path.match(/^\/api\/admin\/maintenance\/([\w-]+)\/cancel$/)
  if (cancel) {
    await db.prepare("DELETE FROM maintenance WHERE id = ?").bind(cancel[1]).run()
    await buildSnapshot(db, now)
    return json({ ok: true })
  }
  return fail(404, "not found")
}

async function subscribe(request: Request, env: Env, now: number): Promise<Response> {
  const body = (await request.json().catch(() => null)) as { email?: unknown } | null
  const email = typeof body?.email === "string" ? body.email.trim().toLowerCase() : ""
  if (email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return fail(400, "enter a valid email address")
  if (!mailEnabled(env)) return fail(503, "email updates are not available")
  const day = `sub:${new Date(now).toISOString().slice(0, 10)}`
  const sent = Number((await env.DB.prepare("SELECT v FROM kv WHERE k = ?").bind(day).first<{ v: string }>())?.v ?? 0)
  if (sent >= 300) return fail(429, "too many sign-ups today, try again tomorrow") // bounds what a stranger can make us mail
  const row = await env.DB.prepare("SELECT token, confirmed, last_mail FROM subscribers WHERE email = ?").bind(email).first<{ token: string; confirmed: number; last_mail: number }>()
  if (row?.confirmed === 1) return json({ ok: true }) // never reveals who is subscribed
  if (row && now - row.last_mail < 600_000) return json({ ok: true })
  const token = row?.token ?? crypto.randomUUID().replace(/-/g, "")
  await env.DB.prepare("INSERT OR IGNORE INTO subscribers (email, token, confirmed, created, last_mail) VALUES (?, ?, 0, ?, 0)").bind(email, token, now).run()
  try { await sendConfirmation(env, email, token) } catch { return fail(502, "the confirmation email could not be sent, try again later") }
  // last_mail marks a mail that really went out: a failed send must not make the next try look like "already sent".
  await env.DB.prepare("UPDATE subscribers SET last_mail = ? WHERE email = ?").bind(now, email).run()
  // The daily quota counts mails that were actually sent: a provider outage must not use it up.
  await env.DB.prepare("INSERT INTO kv (k, v) VALUES (?, '1') ON CONFLICT(k) DO UPDATE SET v = CAST(v AS INTEGER) + 1").bind(day).run()
  return json({ ok: true })
}

async function route(request: Request, env: Env, ctx: Ctx): Promise<Response> {
  const url = new URL(request.url), path = url.pathname, now = Date.now()
  if (path === "/api/status" && request.method === "GET") {
    const snap = await readSnapshot(env.DB)
    if (!snap) return json({ now, at: null, delayed: true, public: null }, 200, "no-store")
    return json({ now, at: snap.at, delayed: now - snap.at > FRESH_MS, public: snap.public }, 200, "public, max-age=15")
  }
  if (path === "/api/feed.xml" && request.method === "GET") {
    const snap = await readSnapshot(env.DB)
    return new Response(feed(env.PUBLIC_ORIGIN ?? "", snap?.public.incidents ?? []), { headers: { "content-type": "application/rss+xml; charset=utf-8", "cache-control": "public, max-age=60" } })
  }
  if (path === "/api/probe" && request.method === "POST") {
    const body = await request.text()
    if (body.length > 64_000) return fail(413, "too large")
    const parsed = await verifyProbe(env, request, body, now)
    if (typeof parsed === "string") return fail(401, parsed)
    const admitted = await admitProbe(env.DB, parsed.vantage, now, parsed.probeTs)
    if (admitted === "replayed") return fail(409, "replayed")
    if (admitted === "rate") return fail(429, "too many reports")
    await saveReport(env.DB, parsed.vantage, now, parsed.probeTs, parsed.netOk, parsed.build, parsed.results)
    return json({ ok: true })
  }
  if (path === "/api/subscribe" && request.method === "POST") return subscribe(request, env, now)
  if (path === "/api/webhook/subscribe" && request.method === "POST") {
    const out = await subscribeWebhook(env, ((await request.json().catch(() => null)) as { url?: unknown } | null)?.url, now)
    return json(out.body, out.status)
  }
  if (path === "/api/webhook/unsubscribe" && request.method === "POST") {
    return (await unsubscribeWebhook(env, ((await request.json().catch(() => null)) as { token?: unknown } | null)?.token)) ? json({ ok: true }) : fail(404, "no such subscription")
  }
  if (path === "/api/confirm" || path === "/api/unsubscribe") {
    // A GET only shows a button: mailbox scanners and link prefetchers GET every link, and must not confirm or unsubscribe anyone.
    const confirm = path === "/api/confirm"
    if (request.method === "GET") return button(confirm ? "Confirm your email" : "Unsubscribe", url.searchParams.get("t") ?? "", path)
    if (request.method === "POST") {
      const token = String((await request.formData().catch(() => null))?.get("t") ?? "")
      const res = confirm ? await env.DB.prepare("UPDATE subscribers SET confirmed = 1 WHERE token = ?").bind(token).run() : await env.DB.prepare("DELETE FROM subscribers WHERE token = ?").bind(token).run()
      return res.meta.changes ? page(confirm ? "You will get an email for incidents and maintenance." : "You are unsubscribed.") : page("This link is no longer valid.")
    }
  }
  if (path.startsWith("/api/admin/")) return admin(request, env, path, now)
  return fail(404, "not found")
}

/** The whole API: one entry for the Pages Function (functions/api/[[path]].ts) and the local dev Worker. */
export async function handle(request: Request, env: Env, ctx: Ctx): Promise<Response> {
  try { return await route(request, env, ctx) } catch (e) {
    console.error("status api", e instanceof Error ? e.message : e)
    return fail(500, "internal error")
  }
}
