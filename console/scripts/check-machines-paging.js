// Browser check for the Machines card and table against the real account's server totals. Run in a
// signed-in tab on / (Personal), e.g. a headless Chromium. It reads GET /api/v1/machines/summary
// and ?state=revoked, and checks: the card's online/offline and per-platform counts are the
// summary's (revoked left out); the table's range or row count is the summary's total; Status →
// Revoked lists the revoked machines. Paging past 50 is check-machines-paging-stub.js. Changes
// nothing. Prints the steps (window.__checkSteps).
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
  const get = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data
  const summary = await get("machines/summary?workspace=personal")
  const revoked = (await get("machines?limit=1&state=revoked&workspace=personal")).pagination.total
  const { total } = summary
  const online = summary.by_state.online
  pass("the server's summary answers", Number.isInteger(total), `${total} in use, ${online} online, ${revoked} revoked`)

  const card = await until(() => {
    const el = [...document.querySelectorAll("section > div")].find((c) => /^Machines/.test(c.innerText.trim()))
    return el?.querySelector("[data-machine-platforms]") && el
  })
  const numbers = card?.innerText.match(/(\d+)\s*Online[\s\S]*?(\d+)\s*Offline/)
  pass(
    "the card shows the summary's online and offline",
    Number(numbers?.[1]) === online && Number(numbers?.[2]) === total - online,
    numbers ? `card ${numbers[1]}/${numbers[2]}, server ${online}/${total - online}` : "none",
  )
  const shown = [...(card?.querySelectorAll("[data-machine-platforms] > span") ?? [])].map((span) => [
    span.querySelector("button")?.getAttribute("aria-label"),
    Number(span.lastElementChild?.textContent),
  ])
  const expected = { linux: 0, macos: 0, windows: 0, ...summary.by_platform }
  pass(
    "the card's platforms are the summary's",
    shown.length === Object.keys(expected).length && shown.reduce((sum, [, n]) => sum + n, 0) === Object.values(expected).reduce((a, b) => a + b, 0),
    shown.map(([name, n]) => `${name} ${n}`).join(", "),
  )

  const rows = () => document.querySelectorAll("#machines-table tbody tr").length
  const range = () => document.querySelector("[data-machine-range]")?.textContent.trim()
  await until(() => rows() > 0 || total === 0)
  pass("the table lists every machine in use", total > 50 ? range()?.endsWith(`of ${total}`) : rows() === total, range() ?? `${rows()} rows`)
  const input = document.querySelector('#machines-table input[placeholder="Search machines"]')
  const type = (text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  type("status:revoked")
  await sleep(1000)
  pass("Status → Revoked lists the revoked machines", revoked > 50 ? range()?.endsWith(`of ${revoked}`) : rows() === revoked, range() ?? `${rows()} rows`)
  type("")
  return steps
})()
