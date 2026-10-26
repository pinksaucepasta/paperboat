// Browser check for the Domains page's hostname paging without real custom domains. Run in a
// signed-in tab on /domains (e.g. a headless Chromium). It stubs GET /api/v1/domains with 130
// tunnel hostnames (cursor, q and kind honoured, like the server) and checks: the hostnames table
// pages 50 at a time with "1–50 of 130", Next and Previous, ?page=2 in the URL, and a search runs
// on the server and starts again at page 1. Changes nothing. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 30000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const items = Array.from({ length: 130 }, (_, i) => ({
    kind: "tunnel",
    domain: { id: `d${i}`, hostname: `h${String(i).padStart(3, "0")}.example.test`, state: "ready", tunnel_id: "t1", owner_name: "stub-tunnel" },
  }))
  const realFetch = window.__realFetch ?? window.fetch
  window.__realFetch = realFetch
  window.fetch = async (input, init) => {
    const url = new URL(typeof input === "string" ? input : input.url, location.href)
    if (url.pathname !== "/api/v1/domains") return realFetch(input, init)
    const q = url.searchParams.get("q") ?? ""
    const offset = Number(url.searchParams.get("cursor") ?? 0)
    const limit = Number(url.searchParams.get("limit") ?? 50)
    const kind = url.searchParams.get("kind")
    const chosen = items.filter((item) => item.domain.hostname.includes(q) && (!kind || item.kind === kind))
    const next = offset + limit < chosen.length ? String(offset + limit) : undefined
    return Response.json({ data: { items: chosen.slice(offset, offset + limit), total: chosen.length, ...(next && { next_cursor: next }) } })
  }
  const range = () => document.querySelector("[data-hostnames] [data-list-range]")?.textContent.trim()
  const button = (label) => [...document.querySelectorAll("[data-hostnames] button")].find((b) => b.textContent.trim() === label)
  try {
    pass("the hostnames table pages the stubbed 130", Boolean(await until(() => range() === "1–50 of 130")), range() ?? "none")
    button("Next")?.click()
    await until(() => range()?.startsWith("51–"), 8000)
    pass("Next shows page 2 with ?page=2", range() === "51–100 of 130" && new URL(location.href).searchParams.get("page") === "2", range() ?? "")
    button("Previous")?.click()
    await until(() => range()?.startsWith("1–"), 8000)
    pass("Previous goes back", range() === "1–50 of 130", range() ?? "")
    const input = document.querySelector('[data-hostnames] input[placeholder="Search hostnames"]')
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, "h12")
    input.dispatchEvent(new Event("input", { bubbles: true }))
    await until(() => !range() && document.querySelectorAll("[data-hostnames] tbody tr").length === 10, 8000)
    pass("search runs on the server, one page", document.querySelectorAll("[data-hostnames] tbody tr").length === 10, `${document.querySelectorAll("[data-hostnames] tbody tr").length} rows`)
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
