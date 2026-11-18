// The day-bar tooltip: one shared element, shown on hover and keyboard focus, with arrow-key roving between days.
let tip: HTMLElement | null = null
const dayFmt = new Intl.DateTimeFormat([], { weekday: "short", month: "short", day: "numeric", year: "numeric", timeZone: "UTC" })

function show(bar: HTMLElement) {
  tip ??= Object.assign(document.createElement("div"), { className: "tip", hidden: true })
  tip.hidden = false
  tip.replaceChildren()
  const b = document.createElement("b")
  b.textContent = dayFmt.format(Date.parse(`${bar.dataset.day}T00:00:00Z`))
  tip.append(b)
  for (const line of (bar.dataset.lines ?? "").split("\n")) { const p = document.createElement("div"); p.textContent = line; tip.append(p) }
  const incidents = (bar.dataset.incidents ?? "").split("\n").filter(Boolean)
  if (incidents.length) {
    const ul = document.createElement("ul")
    for (const t of incidents) { const li = document.createElement("li"); li.textContent = t; ul.append(li) }
    tip.append(ul)
  }
  if (!tip.isConnected) document.body.append(tip)
  const r = bar.getBoundingClientRect(), w = tip.offsetWidth
  tip.style.left = `${Math.max(8, Math.min(innerWidth - w - 8, r.left + r.width / 2 - w / 2))}px`
  tip.style.top = `${Math.max(8, r.top - tip.offsetHeight - 8)}px`
}
const hide = () => { if (tip) tip.hidden = true }

/** A short text tooltip on an info icon (decorative: the same text is in the row's expanded view, which is the keyboard and touch path). */
export function attachInfo(icon: HTMLElement, text: string) {
  icon.addEventListener("pointerenter", () => {
    tip ??= Object.assign(document.createElement("div"), { className: "tip", hidden: true })
    tip.hidden = false
    tip.replaceChildren(document.createTextNode(text))
    if (!tip.isConnected) document.body.append(tip)
    const r = icon.getBoundingClientRect(), w = tip.offsetWidth
    tip.style.left = `${Math.max(8, Math.min(innerWidth - w - 8, r.left + r.width / 2 - w / 2))}px`
    tip.style.top = `${Math.max(8, r.top - tip.offsetHeight - 8)}px`
  })
  icon.addEventListener("pointerleave", hide)
}


export function attachBars(strip: HTMLElement) {
  const bars = () => [...strip.querySelectorAll<HTMLElement>(".bar")]
  strip.addEventListener("pointerover", (e) => { const b = (e.target as HTMLElement).closest<HTMLElement>(".bar"); if (b) show(b) })
  strip.addEventListener("pointerleave", hide)
  strip.addEventListener("focusin", (e) => show(e.target as HTMLElement))
  strip.addEventListener("focusout", hide)
  strip.addEventListener("keydown", (e) => {
    const all = bars(), i = all.indexOf(e.target as HTMLElement)
    const to = e.key === "ArrowLeft" ? i - 1 : e.key === "ArrowRight" ? i + 1 : e.key === "Home" ? 0 : e.key === "End" ? all.length - 1 : -2
    if (to === -2 || !all[to]) return
    e.preventDefault()
    all.forEach((b) => (b.tabIndex = -1))
    all[to].tabIndex = 0
    all[to].focus()
  })
  strip.addEventListener("click", (e) => { const b = (e.target as HTMLElement).closest<HTMLElement>(".bar"); if (b) show(b) }) // tap on touch screens
}
