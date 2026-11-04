// The edge's login page (/.paperboat/access/login): a denied account's handoff is still posted to the edge
// callback (the edge answers 403), and ?switch=1 signs the console session out with a return to the same
// address without switch. The server, sign-out and the edge callback are stubbed; nothing real is changed.
//   CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> node check-access-login.mjs
import { chromium } from "playwright-core"

const { CHROME, CONSOLE_ORIGIN: origin } = process.env
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } })
await ctx.addCookies([{ name: "pb-dev-session", value: "x", url: origin }])
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const transaction = `bat_${"a".repeat(43)}`
const edge = "https://app.example.test"

const posted = []
await ctx.route(new RegExp(`^${edge}/`), (route) => {
  posted.push({ url: route.request().url(), body: route.request().postData() ?? "" })
  return route.fulfill({ status: 403, contentType: "text/html", body: "<h1>No access</h1>" })
})
await ctx.route("**/api/v1/browser-access/issue", (route) =>
  route.fulfill({
    json: { data: { transaction_id: transaction, handoff: `bah_${"b".repeat(43)}`, callback_origin: edge, expires_at: "2099-01-01T00:00:00Z", denied: true } },
  }),
)
const signedOut = []
await ctx.route("**/auth/sign-out", (route) => {
  signedOut.push(route.request().postData() ?? "")
  return route.fulfill({ status: 303, headers: { location: `${origin}/signed-out-stub` } })
})

const page = await ctx.newPage()
await page.goto(`${origin}/.paperboat/access/login?transaction_id=${transaction}`)
await page.waitForURL((url) => url.origin === edge, { timeout: 15000 }).catch(() => {})
log(posted.length === 1 && posted[0].url === `${edge}/.paperboat/access/callback`, "a denied account still posts to the edge callback")
log(posted[0]?.body.includes(`transaction_id=${transaction}`) && posted[0].body.includes("handoff="), "...with the transaction and handoff")
log((await page.getByText("No access").count()) === 1, "...and the edge's 403 page is what the browser shows")

await page.goto(`${origin}/.paperboat/access/login?transaction_id=${transaction}&switch=1&x=y`)
await page.waitForURL(`${origin}/signed-out-stub`, { timeout: 15000 }).catch(() => {})
const form = new URLSearchParams(signedOut[0] ?? "")
log(signedOut.length === 1, "switch=1 signs the console session out once", String(signedOut.length))
log(form.get("return_to") === `/.paperboat/access/login?transaction_id=${transaction}&x=y`, "...returning to the same address without switch", form.get("return_to") ?? "")
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
