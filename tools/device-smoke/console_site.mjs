// Public pages that need no account: the six legal pages of the marketing site (served, titled, dated, linked from the
// footer and from each other, no placeholder left) and the console's /auth/failed (each refusal) and /auth/deleted.
// usage: ORIGIN=<console origin> MARKETING_ORIGIN=<site> [PW_MODULE=...] node console_site.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const site = (process.env.MARKETING_ORIGIN ?? "https://paperboat.cloud").replace(/\/$/, "")
const legal = [["terms", "Terms of Service"], ["privacy", "Privacy Policy"], ["acceptable-use", "Acceptable Use Policy"], ["refunds", "Refund Policy"], ["subprocessors", "Subprocessors"], ["dpa", "Data Processing Addendum"]]
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const open = async (url) => {
  const response = await page.goto(url, { waitUntil: "domcontentloaded", timeout: 120000 }).catch(() => null)
  await page.waitForTimeout(1500)
  return response?.status() ?? 0
}
try {
  await open(site + "/")
  const footer = await page.locator("footer a").evaluateAll((all) => all.map((a) => a.getAttribute("href")))
  const unlinked = legal.filter(([slug]) => !footer.some((href) => href?.replace(/\/$/, "").endsWith(`/legal/${slug}`)))
  log(!unlinked.length, "the site's footer links all six legal pages", unlinked.map((l) => l[0]).join(",") || "all linked")
  for (const [slug, title] of legal) {
    const status = await open(`${site}/legal/${slug}/`)
    const h1 = (await page.locator("h1").first().innerText().catch(() => "")).trim()
    const text = await page.locator("main, body").first().innerText().catch(() => "")
    const updated = await page.locator("time[datetime]").first().getAttribute("datetime").catch(() => null)
    const placeholder = text.match(/not published yet|lorem ipsum|\bTODO\b|\bTBD\b|\[[A-Z][A-Z _]{3,}\]|\{\{/)?.[0]
    log(status === 200 && h1 === title && Boolean(updated) && text.length > 1200 && !placeholder, `/legal/${slug}/ is the dated ${title} with no placeholder`,
      `HTTP ${status}, h1 "${h1}", updated ${updated}, ${text.length} characters${placeholder ? `, placeholder "${placeholder}"` : ""}`)
    const others = await page.locator('a[href*="/legal/"]').evaluateAll((all) => all.map((a) => a.getAttribute("href")))
    log(legal.every(([other]) => other === slug || others.some((href) => href.includes(`/legal/${other}`))), `/legal/${slug}/ links the other policies`)
  }
  for (const [query, heading, extra] of [["", "We couldn’t sign you in", "Sign in"], ["?reason=account_banned&ref=support_row-1", "This account can’t sign in", "support_row-1"], ["?reason=ip_blocked&ref=support_row-2", "Sign-in isn’t available from this network", "support_row-2"]]) {
    const status = await open(`${origin}/auth/failed${query}`)
    await page.getByRole("heading", { name: heading }).waitFor({ timeout: 30000 }).catch(() => {})
    const text = await page.locator("body").innerText()
    log(status === 200 && text.includes(heading) && text.includes(extra), `/auth/failed${query.split("&")[0]} says "${heading}"`, `HTTP ${status}`)
  }
  await open(`${origin}/auth/failed?reason=account_banned&ref=${encodeURIComponent("<script>x</script>")}`)
  await page.getByRole("heading").first().waitFor({ timeout: 30000 }).catch(() => {})
  log(!(await page.locator("body").innerText()).includes("<script>"), "/auth/failed does not show a reference that is not a reference")
  const status = await open(`${origin}/auth/deleted`)
  const text = await page.locator("body").innerText()
  log(status === 200 && /Your account is deleted/.test(text), "/auth/deleted says the account is deleted, signed out", `HTTP ${status}`)
} catch (error) {
  log(false, "site check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
