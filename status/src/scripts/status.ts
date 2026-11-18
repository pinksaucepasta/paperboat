// The status pages' client: fetch the same-origin API every 30 s and draw it. A failed fetch is "Status unavailable", never the last green.
import { overall, type Payload } from "./model.ts"
import { renderBanner, renderHistory, renderMaintenance, renderSystem } from "./render.ts"

async function fetchStatus(): Promise<Payload | null> {
  try {
    const res = await fetch("/api/status", { cache: "no-store" })
    return res.ok ? ((await res.json()) as Payload) : null
  } catch { return null }
}

const $ = (id: string) => document.getElementById(id)

async function refresh() {
  const p = await fetchStatus()
  const o = overall(p)
  const banner = $("banner"), maint = $("maintenance"), system = $("system"), history = $("history-list")
  if (banner) renderBanner(banner, o, p)
  if (maint) renderMaintenance(maint, p?.public?.maintenance ?? [], new Map((p?.public?.components ?? []).map((c) => [c.id, c.name])), p?.now ?? Date.now())
  if (system) renderSystem(system, o, p)
  if (history) renderHistory(history, p, p?.now)
}

void refresh()
setInterval(refresh, 30_000)
