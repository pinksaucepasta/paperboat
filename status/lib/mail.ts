import type { Env, Incident } from "./types.ts"

export const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!)
const origin = (env: Env) => (env.PUBLIC_ORIGIN ?? "").replace(/\/$/, "")
export const mailEnabled = (env: Env) => Boolean(env.MAIL_ENDPOINT && env.MAIL_KEY && env.MAIL_FROM && env.PUBLIC_ORIGIN)

async function send(env: Env, to: string, subject: string, html: string, key: string) {
  const res = await fetch(env.MAIL_ENDPOINT!, {
    method: "POST",
    headers: { authorization: `Bearer ${env.MAIL_KEY}`, "content-type": "application/json", "idempotency-key": key },
    body: JSON.stringify({ from: env.MAIL_FROM, to: [to], subject, html }),
    signal: AbortSignal.timeout(10_000),
  })
  if (!res.ok) throw new Error(`mail provider answered ${res.status}`)
}

export const sendConfirmation = (env: Env, email: string, token: string) =>
  send(env, email, "Confirm your Paperboat status updates",
    `<p>Confirm to get an email when Paperboat has an incident or planned maintenance.</p><p><a href="${origin(env)}/api/confirm?t=${token}">Confirm</a></p>`,
    `confirm-${token}-${Math.floor(Date.now() / 600_000)}`)

/** One mail per confirmed subscriber for an incident update; failures are counted, never thrown at the admin. */
export async function sendIncident(env: Env, subs: { email: string; token: string }[], incident: Incident, update: { status: string; body: string }, key: string) {
  let failed = 0
  for (let i = 0; i < subs.length; i += 10) {
    const done = await Promise.allSettled(subs.slice(i, i + 10).map((s) =>
      send(env, s.email, `[${update.status}] ${incident.title}`,
        `<h2>${esc(incident.title)}</h2><p><b>${esc(update.status)}</b></p><p>${esc(update.body).replace(/\n/g, "<br>")}</p>` +
        `<p><a href="${origin(env)}">${origin(env)}</a> · <a href="${origin(env)}/api/unsubscribe?t=${s.token}">Unsubscribe</a></p>`,
        `${key}-${s.token}`)))
    failed += done.filter((d) => d.status === "rejected").length
  }
  return failed
}
