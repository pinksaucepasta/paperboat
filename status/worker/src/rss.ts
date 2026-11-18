import { esc } from "./mail.ts"
import type { Incident } from "./types.ts"

export function feed(origin: string, incidents: Incident[]): string {
  const items = incidents.flatMap((i) =>
    i.updates.map((u) =>
      `<item><title>${esc(`[${u.status}] ${i.title}`)}</title><guid isPermaLink="false">${esc(`${i.id}-${u.ts}`)}</guid>` +
      `<pubDate>${new Date(u.ts).toUTCString()}</pubDate><link>${esc(origin)}</link><description>${esc(u.body)}</description></item>`))
  return `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Paperboat status</title><link>${esc(origin)}</link>` +
    `<description>Incidents and maintenance</description>${items.join("")}</channel></rss>`
}
