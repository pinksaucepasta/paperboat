// PQ-04 live console check: each list page shows the server's total and offers more only when the server has more.
// Read-only except the relay pool switch, which is turned on and back off. Env: PW_MODULE, PW_CHROMIUM, ORIGIN, STATE.
// Needs an account with more than 50 api tokens, policy versions, servers and enrolled relays. Prints one JSON line per page.
const { chromium } = await import(process.env.PW_MODULE)
const origin = process.env.ORIGIN
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const page = await context.newPage()
const seen = {}
const ids = {}
page.on("response", async (r) => {
  const u = r.url()
  for (const [key, re] of [["tokens", /\/api-tokens\?.*state=active/], ["versions", /\/policy\/versions\?/], ["installations", /\/selfhost\/installations\?(?!.*enrollment_state)/], ["pool", /\/selfhost\/installations\?.*capability=relay.*enrollment_state=enrolled/]]) {
    if (re.test(u)) {
      try {
        const b = await r.json(); const d = b.data ?? b
        const list = d.api_tokens ?? d.versions ?? d.installations ?? []
        const items = list.length
        // Distinct rows fetched so far (refetches repeat pages); a search or filter starts a new set.
        const sig = u.replace(/[?&]cursor=[^&]*/, "")
        ids[key] = ids[key]?.sig === sig ? ids[key] : { sig, set: new Set() }
        for (const x of list) ids[key].set.add(x.id ?? x.version ?? x.installation_id)
        const loaded = ids[key].set.size
        seen[key] = { status: r.status(), url: u.replace(/^https?:\/\/[^/]+/, ""), total: key === "tokens" ? d.active_total : (d.total ?? d.pagination?.total), items, loaded, next: Boolean(d.next_cursor ?? d.pagination?.next_cursor) }
      } catch {}
    }
  }
})
const text = async () => (await page.locator("main").last().innerText().catch(() => "")).replace(/\s+/g, " ")
// The API tokens page is prefetched on the server, so its first page is not a browser request: its count comes from the page
// text ("N of M tokens loaded") and the rows fetched by Load more are added to it.
const report = async (name, key, pattern) => {
  const t = await text()
  const m = t.match(pattern)
  const loadMore = await page.getByRole("button", { name: /^Load more/ }).count()
  const s = seen[key]
  const shownTotal = m ? Number(m[2]) : null
  const ok = key === "tokens" ? Boolean(m && Number(m[1]) === 50 && loadMore > 0) : Boolean(s && s.status === 200 && (s.total === 0 || shownTotal === s.total) && (loadMore > 0) === s.next)
  console.log(JSON.stringify({ page: name, server: s, shown: m ? m[0] : null, loadMore, ok }))
}
// Load more until the server has no more: the count must reach the server's total and the button must go away.
const walk = async (name, key, pattern) => {
  const steps = []
  for (let i = 0; i < 6; i++) {
    const m = (await text()).match(pattern)
    const more = page.getByRole("button", { name: /^Load more$/ })
    steps.push(m ? m[0] : null)
    if (!(await more.count())) break
    await more.first().click()
    await page.waitForTimeout(2000)
  }
  // The "N of M loaded" line goes away with the button once the server has no more: then every row must have been fetched.
  const first = steps[0]?.match(pattern)
  const s = key === "tokens" ? { total: first ? Number(first[2]) : 0, loaded: (first ? Number(first[1]) : 0) + (seen[key]?.loaded ?? 0), next: false } : seen[key]
  console.log(JSON.stringify({ page: name, steps, serverTotal: s?.total, loaded: s?.loaded, ok: Boolean(s && s.total > 50 && s.loaded === s.total && !s.next && first && first[1] === "50") }))
}
await page.goto(origin + "/settings/api-tokens", { waitUntil: "networkidle", timeout: 60000 })
await page.waitForTimeout(2500)
await report("api tokens", "tokens", /(\d+) of (\d+) tokens/)
await walk("api tokens load more", "tokens", /(\d+) of (\d+) tokens/)
await page.goto(origin + "/access", { waitUntil: "networkidle", timeout: 60000 })
await page.getByRole("tab", { name: "History" }).click().catch(() => {})
await page.waitForTimeout(2500)
await report("policy history", "versions", /(\d+) of (\d+) versions/)
await walk("policy history load more", "versions", /(\d+) of (\d+) versions/)
await page.goto(origin + "/network", { waitUntil: "domcontentloaded", timeout: 60000 })
await page.waitForTimeout(6000)
const range = async () => (await page.locator("[data-list-range]").first().innerText().catch(() => "")).replace(/\s+/g, " ")
const next = page.getByRole("button", { name: "Next", exact: true })
const previous = page.getByRole("button", { name: "Previous", exact: true })
// Page controls under the servers table: 1-50 -> Next -> 51-N (Next off) -> Previous -> 1-50; the server's total is the count.
const total = seen.installations?.total
const first = await range()
await next.click()
await page.waitForTimeout(2500)
const second = await range()
const nextOff = await next.isDisabled()
await previous.click()
await page.waitForTimeout(2500)
const back = await range()
const lastFrom = Math.floor((total - 1) / 50) * 50 + 1
console.log(JSON.stringify({ page: "network", serverTotal: total, first, second, nextOffOnLast: nextOff, back,
  ok: Boolean(total > 50 && first === `1–50 of ${total}` && second === `${lastFrom}–${total} of ${total}` && nextOff && back === first) }))
// Search is the server's: the pager resets to page 1 of the matches.
await page.getByPlaceholder("Search servers").fill("pq04s-0")
await page.waitForTimeout(3000)
const hits = seen.installations
console.log(JSON.stringify({ page: "network search", server: hits, shown: await range(), ok: Boolean(hits?.url.includes("q=pq04s-0") && hits.total > 0 && hits.total < total) }))
// Pool picker: the relay pool reads enrolled relays from the server (limit 50, cursor); the switch saves at most the pool's 32.
const s = seen.pool
const switchEl = page.getByRole("switch").first()
const saved = page.waitForResponse((r) => /\/selfhost\/pools\/relay/.test(r.url()) && r.request().method() !== "GET", { timeout: 20000 })
await switchEl.click()
const put = await (await saved).json().catch(() => null)
const pool = put?.data ?? put
console.log(JSON.stringify({ page: "pool picker", server: s, saved: { mode: pool?.mode, ids: pool?.installation_ids?.length },
  ok: Boolean(s && s.status === 200 && s.url.includes("limit=50") && s.items === 50 && s.next && pool?.mode === "self-hosted-only" && pool.installation_ids?.length === 32) }))
const restored = page.waitForResponse((r) => /\/selfhost\/pools\/relay/.test(r.url()) && r.request().method() !== "GET", { timeout: 20000 })
await switchEl.click()
const back2 = await (await restored).json().catch(() => null)
console.log(JSON.stringify({ page: "pool picker restore", mode: (back2?.data ?? back2)?.mode }))
await browser.close()
