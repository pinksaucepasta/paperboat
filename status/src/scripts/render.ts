// DOM builders for the status page. Every dynamic string goes through textContent, never innerHTML.
import { BANNER, PHASES, PHASE_WORD, STATE_WORD, byMonth, dayLines, duration, incidentsOn, kindOf, phaseIndex, shownState, todayUtc, trackedDays } from "./model.ts"
import type { Component, Incident, Maintenance, Overall, Payload, State } from "./model.ts"
import { attachBars, attachInfo } from "./tooltip.ts"

export const el = <K extends keyof HTMLElementTagNameMap>(tag: K, cls = "", text?: string): HTMLElementTagNameMap[K] => {
  const e = document.createElement(tag)
  if (cls) e.className = cls
  if (text !== undefined) e.textContent = text
  return e
}

const NS = "http://www.w3.org/2000/svg"
const ICON: Record<string, [string, string]> = { // [colour class, inner svg paths]
  operational: ["ok", '<circle cx="10" cy="10" r="9" fill="currentColor"/><path d="M6 10.4l2.8 2.8L14 7.6" fill="none" stroke="#fff" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>'],
  degraded: ["warn", '<path d="M10 1.8l8.6 15.4H1.4z" fill="currentColor" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><path d="M10 7.6v4.2M10 14.2v.1" stroke="#fff" stroke-width="1.8" stroke-linecap="round"/>'],
  major: ["bad", '<circle cx="10" cy="10" r="9" fill="currentColor"/><path d="M6.8 6.8l6.4 6.4M13.2 6.8l-6.4 6.4" stroke="#fff" stroke-width="1.8" stroke-linecap="round"/>'],
  maintenance: ["maint", '<circle cx="10" cy="10" r="9" fill="currentColor"/><path d="M10 5.5v5l3 1.8" fill="none" stroke="#fff" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>'],
  delayed: ["none", '<circle cx="10" cy="10" r="8.2" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M10 5.5v5l3 1.8" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>'],
  unknown: ["none", '<circle cx="10" cy="10" r="8.2" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M7.8 8a2.3 2.3 0 114 1.5c-.8.7-1.8 1-1.8 2.3M10 14.6v.1" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/>'],
}
export function icon(state: string): SVGSVGElement {
  const [cls, paths] = ICON[state] ?? ICON.unknown
  const svg = document.createElementNS(NS, "svg")
  svg.setAttribute("viewBox", "0 0 20 20")
  svg.setAttribute("class", `ico ${cls}`)
  svg.setAttribute("aria-hidden", "true")
  svg.innerHTML = paths // a constant above, no data
  return svg
}
const info = () => { const s = document.createElementNS(NS, "svg"); s.setAttribute("viewBox", "0 0 16 16"); s.setAttribute("aria-hidden", "true"); s.innerHTML = '<circle cx="8" cy="8" r="6.5" fill="none" stroke="currentColor" stroke-width="1.3"/><path d="M8 7.2v3.6M8 5.2v.1" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>'; return s }
const chevron = () => { const s = document.createElementNS(NS, "svg"); s.setAttribute("viewBox", "0 0 14 14"); s.setAttribute("aria-hidden", "true"); s.innerHTML = '<path d="M5 3l4 4-4 4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>'; return s }

const tone = (s: string) => ({ operational: "", degraded: "warn", major: "bad", maintenance: "maint", delayed: "none", unknown: "none" })[s] ?? "none"

const dateFmt = new Intl.DateTimeFormat([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" })
const dayFmt = new Intl.DateTimeFormat([], { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" })
const sinceFmt = new Intl.DateTimeFormat([], { day: "numeric", month: "short", timeZone: "UTC" })
const monthFmt = new Intl.DateTimeFormat([], { month: "long", year: "numeric", timeZone: "UTC" })
export const when = (ms: number) => dateFmt.format(ms)
const time = (ms: number, text = when(ms)) => { const t = el("time", "", text); t.dateTime = new Date(ms).toISOString(); return t }

export function renderBanner(root: HTMLElement, o: Overall, p: Payload | null) {
  const copy = BANNER[o], now = p?.now ?? Date.now()
  const card = el("div", `card banner is-${o}`)
  const head = el("div", "banner-head")
  const line = el("div", "line")
  line.append(icon(o === "unavailable" ? "unknown" : o === "incident" ? "degraded" : o), el("h1", "banner-title", copy.title))
  head.append(line)
  if (copy.detail) head.append(el("p", "banner-detail", copy.detail))
  card.append(head)
  const open = (p?.public?.incidents ?? []).filter((i) => i.status !== "resolved")
  if (open.length && o !== "delayed" && o !== "unavailable") {
    const body = el("div", "banner-body")
    const names = new Map(p!.public!.components.map((c) => [c.id, c.name]))
    const chips = el("div", "affected")
    for (const id of new Set(open.flatMap((i) => i.components))) chips.append(el("span", `chip${p!.public!.components.find((c) => c.id === id)?.state === "major" ? " is-major" : ""}`, names.get(id) ?? id))
    body.append(chips)
    for (const i of open) body.append(issue(i, names, now))
    card.append(body)
  }
  root.replaceChildren(card)
}

function issue(i: Incident, names: Map<string, string>, now: number): HTMLElement {
  const box = el("div", "issue")
  const title = el("div", "issue-title")
  title.append(icon("degraded"), el("span", "", i.title))
  const last = i.updates[0]
  box.append(title)
  if (last) box.append(el("p", "issue-text", last.body))
  const meta = el("p", "issue-meta")
  meta.append(`${PHASE_WORD[i.status] ?? i.status} · Ongoing for ${duration(now - i.created)} · Affects ${i.components.map((c) => names.get(c) ?? c).join(", ")} · `)
  const link = el("a", "", "Details"); link.href = `/history/#${i.id}`
  meta.append(link)
  box.append(meta)
  return box
}

export function renderMaintenance(root: HTMLElement, items: Maintenance[], names: Map<string, string>, now: number) {
  root.replaceChildren()
  const shown = items.filter((m) => m.ends >= now)
  if (!shown.length) return
  const card = el("div", "card")
  const head = el("div", "card-head"); head.append(el("h2", "card-title", "Scheduled maintenance")); card.append(head)
  for (const m of shown) {
    const box = el("div", "item")
    const state = m.starts <= now ? "In progress" : "Scheduled"
    const row = el("div", "item-title"); row.append(el("span", `pill`, state), document.createTextNode(` ${m.title}`))
    const win = el("p", "item-meta"); win.append(time(m.starts), " to ", time(m.ends), ` · ${m.components.map((c) => names.get(c) ?? c).join(", ")}`)
    box.append(row, win)
    if (m.body) box.append(el("p", "issue-text", m.body))
    card.append(box)
  }
  root.append(card)
}

function row(c: Component, o: Overall, p: Payload, ix: number): HTMLElement {
  const shown = shownState(o, c)
  const box = el("div", "comp")
  const head = el("button", "comp-head"); head.type = "button"
  const panel = el("div", "units"); panel.id = `units-${c.id}`; panel.hidden = true
  head.setAttribute("aria-expanded", "false"); head.setAttribute("aria-controls", panel.id)
  head.append(icon(shown), el("span", "comp-name", c.name))
  const hint = el("span", "comp-info"); hint.setAttribute("aria-hidden", "true"); hint.append(info()); attachInfo(hint, c.note); head.append(hint)
  if (shown === "operational") { head.append(Object.assign(el("span", "visually-hidden", "Operational"))) }
  else head.append(el("span", `comp-state ${tone(shown)}`, STATE_WORD[shown]))
  const more = el("span", "comp-more"); more.append(chevron())
  if (c.units.length > 1) more.prepend(`${c.units.length} checks`)
  head.append(more)
  head.append(el("span", "comp-up", shown === "unknown" ? "—" : c.uptime === null ? "Collecting data" : `${c.uptime}% uptime`))
  head.addEventListener("click", () => { const open = head.getAttribute("aria-expanded") !== "true"; head.setAttribute("aria-expanded", String(open)); panel.hidden = !open })
  box.append(head)
  const tracked = trackedDays(c.days, p.now)
  const days = tracked.days
  const today = todayUtc(p.now)
  const slots = days.length > 30 ? 90 : 30 // a short 30-day view until there is more than a month to show; bars stay right-aligned, never padded
  const bars = el("div", "bars")
  bars.style.setProperty("--slots", String(slots))
  bars.setAttribute("role", "group")
  bars.setAttribute("aria-label", `${c.name}: daily status${tracked.full ? " for the last 90 days" : ` since ${sinceFmt.format(Date.parse(`${tracked.since}T00:00:00Z`))}`}. Use the arrow keys to move between days.`)
  days.forEach((d, i) => {
    const isToday = d.day === today
    const b = el("button", `bar ${kindOf(d)}${isToday ? " today" : ""}`); b.type = "button"; b.tabIndex = i === days.length - 1 ? 0 : -1
    b.style.gridColumn = String(slots - days.length + i + 1) // right-aligned: nothing is drawn before tracking started
    const inc = incidentsOn(d.day, c.id, p.public!.incidents, p.now)
    const lines = [...(isToday ? ["Today so far"] : []), ...dayLines(d)]
    b.dataset.day = d.day
    b.dataset.lines = lines.join("\n")
    b.dataset.incidents = inc.map((x) => x.title).join("\n")
    b.setAttribute("aria-label", `${dayFmt.format(Date.parse(`${d.day}T00:00:00Z`))}: ${lines.join(", ")}${inc.length ? `. Incidents: ${inc.map((x) => x.title).join(", ")}` : ""}`)
    bars.append(b)
  })
  if (days.length) attachBars(bars)
  box.append(bars)
  panel.append(el("p", "comp-note", c.note))
  for (const u of c.units) {
    const us = shownState(o, u)
    const li = el("div", "unit"); li.append(icon(us), el("span", "", u.name), el("span", "unit-state", STATE_WORD[us]))
    panel.append(li)
  }
  box.append(panel)
  void ix
  return box
}

export function renderSystem(root: HTMLElement, o: Overall, p: Payload | null) {
  const open = new Set([...root.querySelectorAll(".comp-head[aria-expanded=true]")].map((h) => h.getAttribute("aria-controls"))) // a refresh keeps expanded rows open
  root.replaceChildren()
  if (!p?.public || o === "unavailable") return
  const card = el("div", "card")
  const sinces = p.public.components.map((c) => trackedDays(c.days, p.now)).filter((t) => t.since !== null)
  const earliest = sinces.map((t) => t.since as string).sort()[0]
  const range = !earliest ? "" : sinces.every((t) => t.full) ? "Last 90 days" : `Since ${sinceFmt.format(Date.parse(`${earliest}T00:00:00Z`))}`
  const head = el("div", "card-head"); const title = el("div", "card-title-row"); title.append(el("h2", "card-title", "System status"), el("span", "card-range", range)); head.append(title)
  const live = el("span", "updated"); live.id = "updated"; live.append(el("span", "live-dot"), el("span", "updated-text", "")); head.append(live)
  card.append(head)
  p.public.components.forEach((c, i) => card.append(row(c, o, p, i)))
  root.append(card)
  for (const id of open) { const h = card.querySelector<HTMLElement>(`[aria-controls="${id}"]`); if (h) h.click() }
}

/** One incident as a native <details>: phases, then the timeline of updates, newest first. */
export function incidentDetails(i: Incident, names: Map<string, string>, now: number): HTMLElement {
  const d = el("details", "incident"); d.id = i.id
  const sum = el("summary")
  sum.append(icon(i.status === "resolved" ? "operational" : "degraded"))
  const t = el("span", "inc-title", i.title); sum.append(t)
  sum.append(el("span", "inc-when", `${dayFmt.format(i.created)}${i.resolved ? ` · ${duration(i.resolved - i.created)}` : " · ongoing"}`))
  d.append(sum)
  const ol = el("ol", "phases"), at = phaseIndex(i.status)
  PHASES.forEach((ph, ix) => ol.append(el("li", ix < at ? "is-done" : ix === at ? "is-current" : "", PHASE_WORD[ph])))
  ol.setAttribute("aria-label", "Incident phases")
  d.append(ol)
  d.append(el("p", "item-meta", `Affects ${i.components.map((c) => names.get(c) ?? c).join(", ")}`))
  d.lastElementChild!.setAttribute("style", "margin: 0 var(--s5) var(--s3)")
  const tl = el("div", "timeline")
  for (const u of i.updates) {
    const box = el("div", `update${u.status === "resolved" ? " is-resolved" : ""}`)
    const head = el("div"); head.append(el("span", "update-phase", PHASE_WORD[u.status] ?? u.status), document.createTextNode(" · "), Object.assign(time(u.ts), { className: "update-time" }))
    box.append(head, el("p", "issue-text", u.body))
    tl.append(box)
  }
  d.append(tl)
  void now
  return d
}

export function renderHistory(root: HTMLElement, p: Payload | null, now = Date.now()) {
  root.replaceChildren()
  if (!p?.public) { root.append(el("p", "empty", "History is unavailable right now.")); return }
  const names = new Map(p.public.components.map((c) => [c.id, c.name]))
  const groups = new Map(byMonth(p.public.incidents).map((g) => [g.month, g.items]))
  const months: string[] = []
  for (let i = 0; i < 3; i++) { const d = new Date(Date.UTC(new Date(now).getUTCFullYear(), new Date(now).getUTCMonth() - i, 1)); months.push(d.toISOString().slice(0, 7)) }
  for (const m of new Set([...months, ...groups.keys()])) {
    const card = el("div", "card")
    card.append(el("h2", "month", monthFmt.format(Date.parse(`${m}-01T00:00:00Z`))))
    const items = groups.get(m) ?? []
    if (!items.length) card.append(el("p", "empty", "No incidents reported."))
    for (const i of items) card.append(incidentDetails(i, names, now))
    root.append(card)
  }
  const target = location.hash.slice(1) && document.getElementById(location.hash.slice(1))
  if (target instanceof HTMLDetailsElement) { target.open = true; target.scrollIntoView() }
}

export type { State }
