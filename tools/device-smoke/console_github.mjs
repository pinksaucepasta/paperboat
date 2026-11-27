// GitHub connect as far as a script can go without a person's GitHub sign-in: Config sync's "Connect GitHub" leaves for
// GitHub's authorization page for this console (client id, state, a redirect back to the server or console), and the
// three pages GitHub's flow returns to refuse what is not theirs: /github/connect without a link and with a forged one,
// /github/callback with a forged code and state. Nothing becomes connected. The authorization itself is not covered.
// usage: ORIGIN=... STATE=<a throwaway account> [PW_MODULE=...] node console_github.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const status = async () => (await (await context.request.get(`${origin}/api/v1/github/status`)).json()).data
const says = async (pattern, ms = 60000) => {
  for (const end = Date.now() + ms; Date.now() < end; await page.waitForTimeout(500)) {
    const text = (await page.locator("body").innerText().catch(() => "")).replace(/\s+/g, " ")
    if (pattern.test(text)) return text.slice(0, 200)
  }
  return ""
}
try {
  log((await status())?.connected === false, "the new account has no GitHub connection")
  await page.goto(`${origin}/config`, { waitUntil: "domcontentloaded", timeout: 120000 })
  await page.locator('[data-github="disconnected"]').filter({ visible: true }).first().waitFor({ timeout: 60000 })
  log(true, "Config sync shows the GitHub account as not connected")
  let authorize = null
  await context.route("https://github.com/**", async (route) => {
    authorize ??= new URL(route.request().url())
    await route.fulfill({ status: 200, contentType: "text/html", body: "<p>stopped before GitHub</p>" })
  })
  const startReply = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/v1/github/oauth/start", { timeout: 60000 }).catch(() => null)
  await page.getByRole("button", { name: "Connect GitHub" }).filter({ visible: true }).first().click({ timeout: 30000 })
  const reply = await startReply
  const replyBody = await reply?.json().catch(() => null)
  log(reply?.ok() === true, "the server starts a GitHub sign-in for this console", reply ? `HTTP ${reply.status()} ${replyBody?.error?.code ?? ""} ${replyBody?.error?.support_reference ?? ""} ${replyBody?.data?.authorization_url ? new URL(replyBody.data.authorization_url).host + new URL(replyBody.data.authorization_url).pathname : ""}`.trim() : "no request to oauth/start")
  for (let i = 0; i < 60 && !authorize; i++) await page.waitForTimeout(500)
  log(authorize?.pathname === "/login/oauth/authorize", "Connect GitHub leaves for GitHub's authorization page", authorize ? authorize.pathname : "no request to github.com")
  const redirect = authorize?.searchParams.get("redirect_uri") ?? ""
  log(Boolean(authorize?.searchParams.get("client_id")) && (authorize?.searchParams.get("state") ?? "").length >= 20, "the request carries the app's client id and a fresh state")
  log(/^https:\/\//.test(redirect), "GitHub is told to come back over HTTPS", redirect ? new URL(redirect).host + new URL(redirect).pathname : "no redirect_uri")
  // GitHub's own check of the client id and redirect address: a real request, signed out, ends at its sign-in page.
  if (authorize) {
    const real = await fetch(authorize, { redirect: "manual", signal: AbortSignal.timeout(30000) })
    const to = real.headers.get("location") ?? ""
    log(real.status === 302 && /\/login\?/.test(to) && !/redirect_uri_mismatch|error=/.test(to), "GitHub accepts the app and the redirect address (it asks the person to sign in)", `HTTP ${real.status}`)
  }
  await context.unroute("https://github.com/**")
  await page.goto(`${origin}/github/connect`, { waitUntil: "domcontentloaded" })
  log(Boolean(await says(/This link is incomplete/)), "/github/connect without a link says the link is incomplete")
  await page.goto(`${origin}/github/connect?link=00000000-0000-0000-0000-000000000000#not-a-real-token`, { waitUntil: "domcontentloaded" })
  const refused = await says(/Run the pb command again|expired|not found|can.t be used|isn.t valid|unavailable/i)
  log(Boolean(refused) && !/Approve/.test(refused), "/github/connect refuses a forged link and offers nothing to approve", refused)
  await page.goto(`${origin}/github/callback?code=forged-code&state=forged-state`, { waitUntil: "domcontentloaded" })
  await page.locator('[data-github-callback="error"]').waitFor({ timeout: 60000 }).catch(() => {})
  const answer = await says(/GitHub not connected/)
  log(Boolean(answer), "/github/callback refuses a forged code and state", answer)
  log((await status())?.connected === false, "and nothing became connected")
} catch (error) {
  log(false, "GitHub connect check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
