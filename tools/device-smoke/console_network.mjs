// F9-06, the console half: the machine page's Network section shows every edge and relay time the server measured for it
// and none it did not, each beside its node, and the nodes are the ones `pb edge list` and `pb relay list` name.
// usage: ORIGIN=... STATE=... CLI_JSON='{"edges": pb edge list regions, "relays": pb relay list relays}' [PW_MODULE=...] node console_network.mjs <machine id>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const id = process.argv[2]
const cli = JSON.parse(process.env.CLI_JSON ?? "{}")
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const latency = async () => {
  const reply = await context.request.get(`${origin}/api/v1/machines/${id}/network-latency?workspace=personal`)
  if (!reply.ok()) throw new Error(`network-latency answered ${reply.status()}`)
  return (await reply.json()).data.items
}
try {
  // A new machine reports its first measurements after a few minutes.
  let items = []
  for (const end = Date.now() + 420000; Date.now() < end; await new Promise((r) => setTimeout(r, 15000))) {
    items = await latency()
    if (items.some((i) => i.role === "edge") && items.some((i) => i.role === "relay")) break
  }
  log(items.some((i) => i.role === "edge"), "the server has a measured edge time for the machine", JSON.stringify(items.map((i) => [i.role, i.region ?? i.name ?? "", i.rtt_ms])))
  log(items.some((i) => i.role === "relay"), "the server has a measured relay time for the machine")
  log(items.every((i) => i.rtt_ms > 0 && i.rtt_ms < 5000), "every time is a real measurement (0 < ms < 5000)")
  let problem = ""
  let text = ""
  for (let attempt = 0; attempt < 3; attempt++) {
    await page.goto(`${origin}/machines/${id}`, { waitUntil: "domcontentloaded", timeout: 120000 })
    const section = page.locator("section", { has: page.getByRole("heading", { name: "Network", exact: true }) }).last()
    await section.getByText(/\d+\s*ms/).first().waitFor({ timeout: 60000 }).catch(() => {})
    await page.waitForTimeout(2000)
    text = (await section.innerText()).replace(/\s+/g, " ")
    const now = await latency()
    const shown = [...text.matchAll(/(\d+)\s*ms/g)].map((m) => Number(m[1])).sort((a, b) => a - b)
    const reported = now.map((i) => i.rtt_ms).sort((a, b) => a - b)
    problem = JSON.stringify(shown) === JSON.stringify(reported) ? "" : `server ${reported}, page ${shown}`
    if (!problem) break
  }
  log(!problem, "the page shows exactly the times the server measured", problem || text.slice(0, 200))
  // pb edge list answers {regions:[{id,name,status}]}, pb relay list {relays:[{region,name,status}]}
  const regions = { edge: (cli.edges ?? []).map((r) => r.id), relay: (cli.relays ?? []).map((r) => r.region) }
  log(regions.edge.length > 0 && regions.relay.length > 0, "pb edge list and pb relay list name regions", `edges ${regions.edge}, relays ${regions.relay}`)
  const measured = await latency()
  const unknown = measured.filter((i) => !regions[i.role]?.includes(i.region))
  log(measured.length > 0 && !unknown.length, "every region the console times is one pb lists for that role", unknown.map((i) => `${i.role} ${i.region}`).join(",") || "all listed")
  const named = measured.filter((i) => !text.includes(i.name))
  log(measured.length > 0 && !named.length, "the page names every timed region", named.map((i) => i.name).join(",") || [...new Set(measured.map((i) => i.name))].join(","))
} catch (error) {
  log(false, "network check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
