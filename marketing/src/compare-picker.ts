/**
 * The /compare picker: two menus choose which products stand beside Paperboat. The choice lives in
 * the URL (?vs=tailscale,ngrok) so a comparison can be shared, and the server-rendered page shows
 * the default until this runs. Also keeps the sticky column headings aligned with the columns when
 * they swipe sideways on a phone.
 */
export function mountCompare(root: HTMLElement) {
  const body = root.querySelector<HTMLElement>("[data-body]")!
  const track = root.querySelector<HTMLElement>("[data-heads-track]")!
  body.addEventListener("scroll", () => (track.style.transform = `translateX(${-body.scrollLeft}px)`), {
    passive: true,
  })
  if (!root.hasAttribute("data-picker")) return

  const menus = [...root.querySelectorAll<HTMLSelectElement>("select[data-pick]")]
  const known = new Set([...menus[0].options].map((option) => option.value))

  const show = (picks: string[]) => {
    root.querySelectorAll<HTMLElement>("[data-col]").forEach((element) => {
      const at = picks.indexOf(element.dataset.col!)
      element.hidden = at < 0
      element.style.order = String(at < 0 ? 9 : at + 2)
    })
    menus[0].value = picks[0]
    menus[1].value = picks[1] ?? ""
  }

  const fromUrl = () => {
    const asked = (new URLSearchParams(location.search).get("vs") ?? "").split(",").filter((s) => known.has(s))
    return [...new Set(asked)].slice(0, 2)
  }

  const apply = (picks: string[], push: boolean) => {
    show(picks)
    if (!push) return
    const url = new URL(location.href)
    url.searchParams.set("vs", picks.join(","))
    history.replaceState(null, "", url)
  }

  menus.forEach((menu) =>
    menu.addEventListener("change", () => {
      const first = menus[0].value
      const second = menus[1].value === first ? "" : menus[1].value
      apply(second ? [first, second] : [first], true)
    }),
  )

  const initial = fromUrl()
  if (initial.length) apply(initial, false)
}
