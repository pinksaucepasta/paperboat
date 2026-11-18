// Renders the Worker's /api/status (API_BASE from config.js; empty means the same origin).
import { API_BASE } from "/config.js"
// Renders /api/status. The page never shows green it cannot back: data older than 3 minutes, or a failed fetch, is "unknown".
const STALE_MS = 180000
const LABEL = { operational: "Operational", degraded: "Degraded", major: "Major outage", maintenance: "Maintenance", delayed: "Status data delayed", unknown: "Unknown" }
const BANNER = { operational: "All systems operational", degraded: "Some systems are degraded", major: "Major outage", maintenance: "Scheduled maintenance", delayed: "Status data delayed", unknown: "Status unavailable" }
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e }
const when = (ms) => new Date(ms).toLocaleString([], { dateStyle: "medium", timeStyle: "short" })
const $ = (id) => document.getElementById(id)

function overall(components) {
  const s = components.map((c) => c.state)
  return ["major", "degraded", "maintenance", "delayed"].find((x) => s.includes(x)) ?? "operational"
}

function bars(c) {
  const wrap = el("div"), row = el("div", "bars")
  const byDay = new Map(c.days.map((d) => [d.day, d]))
  for (let i = 89; i >= 0; i--) {
    const day = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10), d = byDay.get(day)
    const kind = !d || d.up + d.degraded + d.down === 0 ? "none" : d.down >= 10 ? "down" : d.down > 0 || d.degraded >= 10 ? "degraded" : "up"
    const b = el("div", `bar ${kind}`)
    b.title = d ? `${day}: ${d.down} min down, ${d.degraded} min degraded` : `${day}: no data`
    row.append(b)
  }
  const axis = el("div", "axis")
  axis.append(el("span", "", "90 days ago"), el("span", "", c.uptime === null ? "No data yet" : `${c.uptime}% uptime`), el("span", "", "Today"))
  wrap.append(row, axis)
  return wrap
}

function component(c, shown) {
  const box = el("div", "component"), head = el("div", "row")
  head.append(el("span", "name", c.name))
  const st = el("span", "state"); st.append(el("span", `dot ${shown}`), document.createTextNode(LABEL[shown]))
  head.append(st)
  box.append(head, el("p", "note", c.note), bars(c))
  if (c.units.length > 1) {
    const ul = el("ul", "units")
    for (const u of c.units) { const li = el("li"); li.append(el("span", "", u.name)); const s = el("span", "state"); s.append(el("span", `dot ${shown === "unknown" ? "unknown" : u.state}`), document.createTextNode(shown === "unknown" ? LABEL.unknown : LABEL[u.state])); li.append(s); ul.append(li) }
    box.append(ul)
  }
  return box
}

function incident(i, names) {
  const card = el("div", "card")
  card.append(el("h3", "", i.title), el("div", "muted", `${i.status === "resolved" ? "Resolved" : "Ongoing"} · ${i.components.map((c) => names.get(c) ?? c).join(", ")}`))
  for (const u of i.updates) { const p = el("p", "update"); p.append(el("b", "", u.status), document.createTextNode(` — ${u.body} `), el("span", "muted", when(u.ts))); card.append(p) }
  return card
}

function render(res) {
  $("feed").href = `${API_BASE}/api/feed.xml`
  const comps = $("components"), active = $("active")
  comps.replaceChildren(); active.replaceChildren(); $("past-list").replaceChildren()
  const data = res.public
  const stale = !data || res.delayed || res.now - res.at > STALE_MS
  if (!data) return fail("Status unavailable")
  const names = new Map(data.components.map((c) => [c.id, c.name]))
  const shown = (c) => (stale ? "unknown" : c.state)
  const state = stale ? "delayed" : overall(data.components)
  $("banner").className = `banner ${state}`
  $("banner-text").textContent = BANNER[state]
  $("updated").textContent = res.at ? `Last check ${when(res.at)}` : ""
  const ongoing = data.incidents.filter((i) => i.status !== "resolved")
  const now = res.now
  for (const m of data.maintenance) { const card = el("div", "card"); card.append(el("h3", "", `${m.starts <= now ? "" : "Planned: "}${m.title}`), el("div", "muted", `${when(m.starts)} to ${when(m.ends)} · ${m.components.map((c) => names.get(c) ?? c).join(", ")}`)); if (m.body) card.append(el("p", "update", m.body)); active.append(card) }
  for (const i of ongoing) active.append(incident(i, names))
  for (const c of data.components) comps.append(component(c, shown(c)))
  const past = data.incidents.filter((i) => i.status === "resolved")
  $("past-list").append(...(past.length ? past.map((i) => incident(i, names)) : [el("p", "muted", "No incidents in the last 14 days.")]))
}

function fail(text) {
  $("banner").className = "banner unknown"
  $("banner-text").textContent = text
  $("components").replaceChildren()
}

async function load() {
  try {
    const r = await fetch(`${API_BASE}/api/status`, { cache: "no-store" })
    if (!r.ok) throw new Error(String(r.status))
    render(await r.json())
  } catch { fail("Status unavailable") }
}

$("sub-form").addEventListener("submit", async (e) => {
  e.preventDefault()
  const btn = e.target.querySelector("button"), msg = $("sub-msg")
  btn.disabled = true
  try {
    const r = await fetch(`${API_BASE}/api/subscribe`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ email: $("email").value }) })
    msg.textContent = r.ok ? "Check your inbox to confirm." : ((await r.json().catch(() => ({}))).error ?? "Could not subscribe.")
  } catch { msg.textContent = "Could not subscribe. Try again." }
  btn.disabled = false
})

load()
setInterval(load, 30000)
