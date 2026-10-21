// Browser check for the Machines page's Hosted allowance and Bandwidth cards (they must agree).
// READ ONLY: paste into the console of a signed-in tab on / (the workspace to check). It reads both
// cards and the server's usage for the same window, and checks that:
//   - the Bandwidth card opens on the billing period (the window the allowance counts),
//   - with the Managed source, its Relay and Edge figures equal the allowance card's, to the digit,
//   - both equal the server's usage for the billing period, formatted the same way,
//   - choosing Last 30 days changes the label and the figures to that window's totals (which may
//     be larger: it reaches back before the billing period).
// Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const units = ["B", "kB", "MB", "GB", "TB", "PB"]
  const formatBytes = (bytes) => {
    let value = bytes
    let unit = 0
    while (value >= 1000 && unit < units.length - 1) {
      value /= 1000
      unit++
    }
    return `${value.toFixed(unit === 0 || value >= 100 ? 0 : 1)} ${units[unit]}`
  }
  const get = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data
  const card = (label) =>
    [...document.querySelectorAll("section > div")].find(
      (c) => c.querySelector("span.font-medium")?.textContent === label,
    )
  const figure = (text, name) => text.match(new RegExp(`${name}\\s*([\\d.]+ [kMGTP]?B)`))?.[1] ?? null
  const flat = (el) => el.innerText.replace(/\s+/g, " ")

  if (location.pathname !== "/") window.next.router.push("/")
  await until(() => location.pathname === "/" && card("Bandwidth"))
  const allowance = await until(
    () => card("Hosted allowance") && /of .* used/.test(flat(card("Hosted allowance"))) && card("Hosted allowance"),
  )
  pass("the Hosted allowance card shows", Boolean(allowance))
  // Wait for the bandwidth card to leave its skeleton.
  await until(
    () => card("Bandwidth") && !card("Bandwidth").querySelector(".animate-pulse") && /\d/.test(flat(card("Bandwidth"))),
  )
  await sleep(1500)

  const billing = await get("billing/workspaces/personal")
  const start = billing.allowance_period_start ?? billing.period_start
  const now = new Date()
  const query = (from) =>
    `usage?${new URLSearchParams({ start: from, end: now.toISOString(), limit: "200", workspace: "personal" })}`
  const managed = (usage, mode) =>
    usage.totals.by_path
      .filter((p) => p.mode === mode && p.hosting === "managed")
      .reduce((sum, p) => sum + p.upload_bytes + p.download_bytes, 0)
  const bandwidth = () => flat(card("Bandwidth"))

  pass(
    "the Bandwidth card opens on the billing period",
    /This billing period|This month/.test(bandwidth()),
    bandwidth().slice(0, 60),
  )
  // Source: Managed.
  const click = (el) => el.dispatchEvent(new MouseEvent("click", { bubbles: true }))
  const choose = async (current, wanted) => {
    const trigger = [...card("Bandwidth").querySelectorAll("button")].find((b) => b.textContent.trim() === current)
    trigger.focus()
    trigger.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }))
    click(trigger)
    const item = await until(() =>
      [...document.querySelectorAll("[role=menuitemradio]")].find((i) => i.textContent.trim() === wanted),
    )
    if (!item) return false
    click(item)
    await sleep(1500)
    return true
  }
  pass("Managed can be chosen", await choose("Combined", "Managed"))
  const allowanceText = flat(allowance)
  const bandwidthText = bandwidth()
  for (const name of ["Relay", "Edge"]) {
    const a = figure(allowanceText, name)
    const b = figure(bandwidthText, name)
    pass(`${name}: both cards show the same figure`, Boolean(a) && a === b, `allowance ${a}, bandwidth ${b}`)
  }
  const since = await get(query(start))
  for (const [name, mode] of [
    ["Relay", "relay"],
    ["Edge", "edge"],
  ]) {
    const want = formatBytes(managed(since, mode))
    pass(
      `${name}: equals the server's usage for the billing period`,
      figure(bandwidthText, name) === want,
      `card ${figure(bandwidthText, name)}, server ${want}`,
    )
  }

  // Last 30 days: the label and the figures follow that window.
  const label = bandwidthText.includes("This billing period") ? "This billing period" : "This month"
  pass("Last 30 days can be chosen", await choose(label, "Last 30 days"))
  const thirty = await get(query(new Date(now.getTime() - 30 * 86400000).toISOString()))
  const text30 = bandwidth()
  for (const [name, mode] of [
    ["Relay", "relay"],
    ["Edge", "edge"],
  ]) {
    const want = formatBytes(managed(thirty, mode))
    pass(
      `${name}: Last 30 days equals the server's 30-day usage`,
      figure(text30, name) === want,
      `card ${figure(text30, name)}, server ${want}`,
    )
  }
  pass("the label names the window", /Last 30 days/.test(text30))
  // Put the card back.
  await choose("Last 30 days", label)
  return steps
})()
