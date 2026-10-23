// Browser check for the Machines page with more machines than one server page (200). Paste into a
// signed-in tab on / (Personal). It reads the server's totals (pagination.total for all machines,
// state=online and state=revoked) and checks: the Machines card shows online and offline for live
// machines only (revoked left out); the table hides revoked machines, pages 50 at a time with a
// range like "51–100 of 266", keeps the page in the URL, and starts again at page 1 on a search;
// Status → Revoked lists exactly the revoked machines. Changes nothing. Prints the steps.
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
  const total = async (state) => {
    const params = new URLSearchParams({ limit: "1", workspace: "personal", ...(state && { state }) })
    return (await (await fetch(`/api/v1/machines?${params}`)).json()).data.pagination.total
  }
  const [all, online, revoked] = await Promise.all([total(), total("online"), total("revoked")])
  const live = all - revoked
  pass("server totals read", all > 0, `${all} records, ${revoked} revoked, ${online} online`)

  const card = await until(() => {
    const el = [...document.querySelectorAll("section > div")].find((c) => /^Machines/.test(c.innerText.trim()))
    return el && /Online/.test(el.innerText) && el
  })
  const numbers = card?.innerText.match(/(\d+)\s*Online[\s\S]*?(\d+)\s*Offline/)
  pass(
    "the card shows the server's online and offline (live machines only)",
    Boolean(numbers) && Number(numbers[1]) === online && Number(numbers[2]) === live - online,
    numbers ? `card ${numbers[1]} online / ${numbers[2]} offline, server ${online} / ${live - online}` : card?.innerText.slice(0, 80),
  )

  const rows = () => document.querySelectorAll("#machines-table tbody tr").length
  const range = () => document.querySelector("[data-machine-range]")?.textContent.trim()
  await until(() => rows() > 0)
  pass("page 1 shows 50 machines", rows() === Math.min(50, live), `${rows()} rows`)
  pass("the range counts live machines", range() === `1–${Math.min(50, live)} of ${live}`, range() ?? "none")

  const next = [...document.querySelectorAll("#machines-table button")].find((b) => b.textContent.trim() === "Next")
  next?.click()
  await until(() => range()?.startsWith("51–"), 5000)
  pass("Next shows the next 50", range() === `51–${Math.min(100, live)} of ${live}`, range() ?? "")
  pass("the page is in the URL", new URL(location.href).searchParams.get("page") === "2")

  const input = document.querySelector('#machines-table input[placeholder="Search machines"]')
  const type = (text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  type("status:revoked")
  await until(() => range()?.startsWith("1–") || rows() <= 50, 5000)
  await sleep(800)
  const revokedRange = range()
  pass(
    "a new filter starts at page 1, and Revoked lists the revoked machines",
    revoked <= 50 ? rows() === revoked && !revokedRange : revokedRange === `1–50 of ${revoked}`,
    revokedRange ?? `${rows()} rows`,
  )
  type("")
  await sleep(800)
  return steps
})()
