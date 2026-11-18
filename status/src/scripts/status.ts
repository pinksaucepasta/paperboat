// The status pages' client: fetch the same-origin API every 30 s and draw it. A failed fetch is "Status unavailable", never the last green.
import { ago, overall, type Payload } from "./model.ts"
import { renderBanner, renderHistory, renderMaintenance, renderSystem } from "./render.ts"

async function fetchStatus(): Promise<Payload | null> {
  try {
    const res = await fetch("/api/status", { cache: "no-store", signal: AbortSignal.timeout(10_000) }) // a hung request is a failure, never "keep the last green"
    return res.ok ? ((await res.json()) as Payload) : null
  } catch { return null }
}

const $ = (id: string) => document.getElementById(id)

// The "Updated 20 s ago" line: age of the last decision, ticking every second between polls. A failed poll says so instead of ticking as if all were well.
let last: { age: number; fetched: number } | null = null
let failed = false
function tick() {
  const text = document.querySelector("#updated .updated-text")
  if (!text) return
  document.getElementById("updated")!.classList.toggle("is-stale", failed)
  text.textContent = !last ? "" : failed ? `Couldn't refresh · last update ${ago(last.age + Date.now() - last.fetched)}` : `Updated ${ago(last.age + Date.now() - last.fetched)}`
}

let busy = false
async function refresh() {
  if (busy) return
  busy = true
  try { await draw() } finally { busy = false }
}

async function draw() {
  const p = await fetchStatus()
  failed = p === null
  if (p?.at != null) last = { age: p.now - p.at, fetched: Date.now() }
  const o = overall(p)
  const banner = $("banner"), maint = $("maintenance"), system = $("system"), history = $("history-list")
  if (banner) renderBanner(banner, o, p)
  if (maint && p?.public) renderMaintenance(maint, p.public.maintenance, new Map(p.public.components.map((c) => [c.id, c.name])), p.now) // a failed poll leaves the known maintenance in place
  if (system) renderSystem(system, o, p)
  if (history) renderHistory(history, p, p?.now)
  tick()
}

void refresh()
setInterval(refresh, 30_000)
setInterval(tick, 1000)
