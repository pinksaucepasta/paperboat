import { sign } from "./probe.ts"
import type { Env, Incident, Maintenance } from "./types.ts"

// Webhook subscriptions, the way incident.io and Statuspage do them: an https URL, a per-subscription secret that signs every delivery
// (header x-paperboat-signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">), a challenge before anything is stored, retries with
// backoff, and removal by token. Delivery is at least once: the delivery id (x-paperboat-delivery) is stable across retries.

export const BACKOFF_MS = [60_000, 300_000, 1_800_000, 7_200_000, 43_200_000] // after attempts 1..5; the 6th failure gives up
export const MAX_ABANDONED = 5 // consecutive given-up deliveries before the subscription is removed
const TIMEOUT_MS = 5000
const DAILY_LIMIT = 100

const hex = (n: number) => [...crypto.getRandomValues(new Uint8Array(n))].map((b) => b.toString(16).padStart(2, "0")).join("")

/** An https URL on a named host: no credentials, no IP literal, no localhost (the Worker cannot reach private addresses anyway). */
export function validUrl(raw: unknown): URL | null {
  if (typeof raw !== "string" || raw.length > 500) return null
  let u: URL
  try { u = new URL(raw) } catch { return null }
  const host = u.hostname
  if (u.protocol !== "https:" || u.username || u.password || host === "localhost" || host.endsWith(".localhost") || !host.includes(".") || /^[\d.]+$/.test(host) || host.includes(":")) return null
  return u
}

async function post(env: Env, url: string, body: string, event: string, delivery: string, secret: string, now: number): Promise<boolean> {
  const t = Math.floor(now / 1000)
  try {
    const res = await fetch(url, {
      method: "POST", redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS),
      headers: { "content-type": "application/json", "user-agent": "paperboat-status-webhook/1", "x-paperboat-event": event, "x-paperboat-delivery": delivery, "x-paperboat-signature": `t=${t},v1=${await sign(secret, `${t}.${body}`)}` },
      body,
    })
    void res.body?.cancel()
    return res.status >= 200 && res.status < 300
  } catch { return false }
}

/** Subscribe: the endpoint must answer the challenge (200 with {"challenge": ...}) before the subscription exists. */
export async function subscribeWebhook(env: Env, rawUrl: unknown, now: number): Promise<{ status: number; body: Record<string, unknown> }> {
  const url = validUrl(rawUrl)
  if (!url) return { status: 400, body: { error: "enter an https address on a public host" } }
  const day = `wh:${new Date(now).toISOString().slice(0, 10)}`
  const used = Number((await env.DB.prepare("SELECT v FROM kv WHERE k = ?").bind(day).first<{ v: string }>())?.v ?? 0)
  if (used >= DAILY_LIMIT) return { status: 429, body: { error: "too many webhook sign-ups today, try again tomorrow" } }
  await env.DB.prepare("INSERT INTO kv (k, v) VALUES (?, '1') ON CONFLICT(k) DO UPDATE SET v = CAST(v AS INTEGER) + 1").bind(day).run()
  if (await env.DB.prepare("SELECT 1 AS x FROM webhooks WHERE url = ?").bind(url.href).first()) return { status: 409, body: { error: "this address is already subscribed" } }
  const challenge = hex(16), id = crypto.randomUUID(), secret = `whsec_${hex(24)}`, token = hex(16)
  const body = JSON.stringify({ type: "verification", challenge })
  const t = Math.floor(now / 1000)
  let answered = false
  try {
    const res = await fetch(url.href, {
      method: "POST", redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS),
      headers: { "content-type": "application/json", "user-agent": "paperboat-status-webhook/1", "x-paperboat-event": "verification", "x-paperboat-signature": `t=${t},v1=${await sign(secret, `${t}.${body}`)}` },
      body,
    })
    answered = res.ok && ((await res.json().catch(() => null)) as { challenge?: string } | null)?.challenge === challenge
  } catch { answered = false }
  if (!answered) return { status: 422, body: { error: 'the address did not answer: reply 200 with {"challenge": "<the challenge from the request>"}' } }
  await env.DB.prepare("INSERT INTO webhooks (id, url, secret, token, created) VALUES (?, ?, ?, ?, ?)").bind(id, url.href, secret, token, now).run()
  return { status: 201, body: { id, secret, unsubscribe_token: token } } // the secret is shown once
}

export async function unsubscribeWebhook(env: Env, token: unknown): Promise<boolean> {
  if (typeof token !== "string" || !/^[0-9a-f]{32}$/.test(token)) return false
  const row = await env.DB.prepare("SELECT id FROM webhooks WHERE token = ?").bind(token).first<{ id: string }>()
  if (!row) return false
  await env.DB.batch([env.DB.prepare("DELETE FROM webhook_queue WHERE webhook_id = ?").bind(row.id), env.DB.prepare("DELETE FROM webhooks WHERE id = ?").bind(row.id)])
  return true
}

export type WebhookEvent = { type: string; data: Incident | Maintenance }

/** Queue one delivery per subscription and try each once now; the cron retries what failed. */
export async function fanout(env: Env, event: WebhookEvent, now: number): Promise<{ queued: number }> {
  const hooks = (await env.DB.prepare("SELECT id FROM webhooks LIMIT 500").all<{ id: string }>()).results
  if (!hooks.length) return { queued: 0 }
  const body = JSON.stringify({ id: crypto.randomUUID(), type: event.type, created: Math.floor(now / 1000), data: event.data })
  await env.DB.batch(hooks.map((h) => env.DB.prepare("INSERT INTO webhook_queue (id, webhook_id, body, attempts, next_at) VALUES (?, ?, ?, 0, ?)").bind(crypto.randomUUID(), h.id, body, now)))
  await deliverDue(env, now)
  return { queued: hooks.length }
}

/** Try every delivery that is due. Success removes it; failure schedules the next try with backoff; the last failure gives up and counts against the subscription. */
export async function deliverDue(env: Env, now: number): Promise<void> {
  const due = (await env.DB.prepare("SELECT q.id, q.webhook_id, q.body, q.attempts, w.url, w.secret FROM webhook_queue q JOIN webhooks w ON w.id = q.webhook_id WHERE q.next_at <= ? ORDER BY q.next_at LIMIT 50").bind(now).all<{ id: string; webhook_id: string; body: string; attempts: number; url: string; secret: string }>()).results
  for (let i = 0; i < due.length; i += 10) {
    await Promise.all(due.slice(i, i + 10).map(async (d) => {
      const event = (JSON.parse(d.body) as { type: string }).type
      if (await post(env, d.url, d.body, event, d.id, d.secret, now)) {
        await env.DB.batch([env.DB.prepare("DELETE FROM webhook_queue WHERE id = ?").bind(d.id), env.DB.prepare("UPDATE webhooks SET abandoned = 0 WHERE id = ?").bind(d.webhook_id)])
      } else if (d.attempts < BACKOFF_MS.length) {
        await env.DB.prepare("UPDATE webhook_queue SET attempts = attempts + 1, next_at = ? WHERE id = ?").bind(now + BACKOFF_MS[d.attempts], d.id).run()
      } else { // given up on this delivery
        await env.DB.batch([
          env.DB.prepare("DELETE FROM webhook_queue WHERE id = ?").bind(d.id),
          env.DB.prepare("UPDATE webhooks SET abandoned = abandoned + 1 WHERE id = ?").bind(d.webhook_id),
          env.DB.prepare("DELETE FROM webhook_queue WHERE webhook_id IN (SELECT id FROM webhooks WHERE id = ? AND abandoned >= ?)").bind(d.webhook_id, MAX_ABANDONED),
          env.DB.prepare("DELETE FROM webhooks WHERE id = ? AND abandoned >= ?").bind(d.webhook_id, MAX_ABANDONED),
        ])
      }
    }))
  }
}
