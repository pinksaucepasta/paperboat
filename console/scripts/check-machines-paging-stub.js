// Browser check for the Machines table's pages, without a large real account. Paste into a
// signed-in tab on / (Personal). It stubs this tab's fetch of GET /api/v1/machines with 130 live
// and 12 revoked machines (cloned from one real record, honouring limit, offset and state), waits
// for the table's next refresh, and checks: the card's online/offline; pages of 50 with the range
// "1–50 of 130", Next to page 3 ("101–130 of 130"), Previous; the page in the URL and a ?page=3 deep
// link; a new search starts at page 1; Status → Revoked lists the 12 revoked. Changes nothing on
// the server; reload the tab afterwards. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 40000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }

  const realFetch = window.__realFetch ?? window.fetch
  window.__realFetch = realFetch
  const sample = (await (await realFetch("/api/v1/machines?limit=1&workspace=personal")).json()).data.items[0]
  const machines = Array.from({ length: 142 }, (_, i) => ({
    ...sample,
    id: `machine_stub_${i}`,
    alias: `stub-${i}`,
    state: i >= 130 ? "revoked" : i < 40 ? "online" : "offline",
    online: i < 40,
  }))
  window.fetch = async (input, init) => {
    const url = new URL(typeof input === "string" ? input : input.url, location.href)
    if (url.pathname !== "/api/v1/machines") return realFetch(input, init)
    const state = url.searchParams.get("state")
    const offset = Number(url.searchParams.get("offset") ?? 0)
    const limit = Number(url.searchParams.get("limit") ?? 200)
    const chosen = state ? machines.filter((m) => m.state === state) : machines
    const items = chosen.slice(offset, offset + limit)
    return Response.json({ data: { items, pagination: { total: chosen.length, offset, limit } } })
  }

  const rows = () => document.querySelectorAll("#machines-table tbody tr").length
  const range = () => document.querySelector("[data-machine-range]")?.textContent.trim()
  const button = (label) => [...document.querySelectorAll("#machines-table button")].find((b) => b.textContent.trim() === label)
  const page = () => new URL(location.href).searchParams.get("page")
  try {
    const first = await until(() => range()?.endsWith("of 130") && range())
    pass("the stubbed 130 live machines are paged", first === "1–50 of 130" && rows() === 50, `${first ?? range() ?? "none"}, ${rows()} rows`)
    const card = [...document.querySelectorAll("section > div")].find((c) => /^Machines/.test(c.innerText.trim()))
    // The card refreshes on its own 15-second interval, apart from the table.
    const read = () => card?.innerText.match(/(\d+)\s*Online[\s\S]*?(\d+)\s*Offline/)
    const numbers = (await until(() => read()?.[1] === "40" && read())) ?? read()
    pass("the card shows 40 online / 90 offline (revoked left out)", numbers?.[1] === "40" && numbers?.[2] === "90", numbers ? `${numbers[1]}/${numbers[2]}` : "none")

    button("Next")?.click()
    await until(() => range()?.startsWith("51–"), 5000)
    button("Next")?.click()
    await until(() => range()?.startsWith("101–"), 5000)
    pass("Next twice reaches page 3", range() === "101–130 of 130" && rows() === 30 && page() === "3", `${range()}, ${rows()} rows, ?page=${page()}`)
    pass("Next is disabled on the last page", button("Next")?.disabled === true)
    button("Previous")?.click()
    await until(() => range()?.startsWith("51–"), 5000)
    pass("Previous goes back to page 2", range() === "51–100 of 130" && page() === "2", `${range()}, ?page=${page()}`)

    // A deep link: leave the page, then open /?page=3; the server's prefetch is real data, so wait
    // for the stubbed refresh.
    window.next.router.push("/config")
    await until(() => location.pathname === "/config", 10000)
    window.next.router.push("/?page=3")
    const deep = await until(() => range() === "101–130 of 130" && range())
    pass("?page=3 opens page 3", Boolean(deep), range() ?? "none")

    const input = document.querySelector('#machines-table input[placeholder="Search machines"]')
    const type = (text) => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
      input.dispatchEvent(new Event("input", { bubbles: true }))
    }
    type("stub")
    await until(() => range()?.startsWith("1–"), 5000)
    pass("a new search starts at page 1", range() === "1–50 of 130", range() ?? "none")
    type("status:revoked")
    await until(() => !range() && rows() === 12, 5000)
    pass("Status → Revoked lists the 12 revoked, one page", !range() && rows() === 12, `${rows()} rows`)
    type("")
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
