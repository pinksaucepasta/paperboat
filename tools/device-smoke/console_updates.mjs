// F14-09, the console half: the update summary is accurate for the workspace. Read-only.
//   1. GET /v1/machines/update-summary (every page) agrees with the machine list: one item per machine, its state is the
//      state of that machine's own last update report ("not_reporting" without one), and `counts` is the histogram.
//   2. The machine table's first page shows each machine's running version and exactly the update mark its report calls
//      for (Update to X, Updating to X, Update to X failed / was rolled back, needs attention, or nothing).
// usage: ORIGIN=<console origin> STATE=<storage state> [PW_MODULE=...] node console_updates.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const get = async (path) => {
  const reply = await context.request.get(`${origin}/api/v1/${path}`, { timeout: 60000 })
  if (!reply.ok()) throw new Error(`GET ${path.split("?")[0]} answered ${reply.status()}`)
  return (await reply.json()).data
}

// the same rules as console/components/machine-update.tsx, written from what a user should see
function mark(machine) {
  const update = machine.update
  if (update?.error_code === "recovery_required" || machine.availability?.update_health === "recovery_required") return "Update needs attention"
  const target = update?.target_version
  if (!target) return ""
  if (["checking", "downloading", "staged", "activating"].includes(update.state)) return `Updating to ${target}`
  if (update.state === "failed") return `Update to ${target} failed`
  if (update.state === "rolled_back") return `Update to ${target} was rolled back`
  return update.state === "deferred" ? `Update to ${target}` : ""
}
const version = (m) => m.runtime_versions?.paperboatd ?? m.runtime_versions?.pb ?? Object.values(m.runtime_versions ?? {})[0] ?? ""

async function summaryAndList() {
  const machines = []
  let cursor = ""
  do {
    const page = await get(`machines?workspace=personal&limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`)
    machines.push(...page.items)
    cursor = page.pagination?.next_cursor ?? page.next_cursor ?? ""
  } while (cursor)
  const items = []
  let counts = {}
  for (let offset = 0; ; ) {
    const page = await get(`machines/update-summary?limit=100&offset=${offset}`)
    items.push(...page.items)
    counts = page.counts
    offset += page.items.length
    const total = page.pagination?.total ?? page.total
    if (!page.items.length || (Number.isFinite(total) && offset >= total)) break
  }
  return { machines, items, counts }
}

try {
  // Machines report while the check reads: a difference must survive a second read to count.
  let problems = []
  let numbers = ""
  for (let attempt = 0; attempt < 3; attempt++) {
    const { machines, items, counts } = await summaryAndList()
    problems = []
    const byId = new Map(machines.map((m) => [m.id, m]))
    const total = Object.values(counts).reduce((a, b) => a + b, 0)
    if (total !== items.length) problems.push(`counts add up to ${total}, ${items.length} items`)
    const histogram = {}
    for (const item of items) histogram[item.state] = (histogram[item.state] ?? 0) + 1
    for (const [state, count] of Object.entries(counts)) if ((histogram[state] ?? 0) !== count) problems.push(`counts.${state}=${count}, items have ${histogram[state] ?? 0}`)
    let compared = 0
    for (const item of items) {
      const machine = byId.get(item.machine_id)
      if (!machine) continue
      compared++
      const own = machine.update?.state ?? "not_reporting"
      if (own !== item.state) problems.push(`${item.alias}: summary ${item.state}, machine list ${own}`)
    }
    const unlisted = machines.filter((m) => !items.some((i) => i.machine_id === m.id))
    if (unlisted.length) problems.push(`${unlisted.length} listed machines are missing from the summary (first: ${unlisted[0].alias})`)
    numbers = `${items.length} in the summary, ${machines.length} listed, ${compared} compared, counts ${JSON.stringify(counts)}`
    if (!problems.length) break
    await new Promise((r) => setTimeout(r, 20000))
  }
  log(!problems.length, "the update summary agrees with the machine list", problems.length ? `${problems.length}: ${problems.slice(0, 3).join("; ")}` : numbers)

  const page = await context.newPage()
  let wrong = []
  let shown = 0
  let marked = 0
  for (let attempt = 0; attempt < 3; attempt++) {
    const answer = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/v1/machines" && r.ok(), { timeout: 60000 }).catch(() => null)
    await page.goto(`${origin}/`, { waitUntil: "domcontentloaded", timeout: 120000 })
    await page.locator("#machines-table tbody tr").first().waitFor({ timeout: 60000 })
    await answer
    await page.waitForTimeout(3000)
    const rows = await page.evaluate(() =>
      [...document.querySelectorAll("#machines-table tbody tr")].map((row) => ({
        href: row.querySelector("a[href^='/machines/']")?.getAttribute("href") ?? "",
        version: row.cells[3]?.innerText.split("\n")[0].trim() ?? "",
        marks: [...(row.cells[3]?.querySelectorAll("[aria-label]") ?? [])].map((e) => e.getAttribute("aria-label")),
      })),
    )
    wrong = []
    shown = 0
    marked = 0
    for (const row of rows) {
      const id = row.href.split("/").pop()
      if (!id) continue
      const machine = await get(`machines/${id}`)
      shown++
      const want = mark(machine)
      if (want) marked++
      if (row.version !== version(machine)) wrong.push(`${machine.alias}: page "${row.version}", server "${version(machine)}"`)
      if ((row.marks[0] ?? "") !== want || row.marks.length > 1) wrong.push(`${machine.alias}: mark "${row.marks.join(",")}", report calls for "${want}"`)
    }
    if (shown && !wrong.length) break
  }
  log(shown > 0 && !wrong.length, "the machine table shows each machine's version and its update mark", wrong.length ? `${wrong.length}: ${wrong.slice(0, 3).join("; ")}` : `${shown} rows, ${marked} with an update mark`)
} catch (error) {
  log(false, "console update check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
