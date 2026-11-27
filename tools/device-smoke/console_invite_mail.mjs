// F12-02-mail-live: the team invitation email arrives, is sent again from the console, and its link opens the console
// Team page after sign-up. The only address invited is a disposable inbox this run creates (console_signup.mjs).
//   run:     node console_invite_mail.mjs run TEAM DIR
//   cleanup: node console_invite_mail.mjs cleanup TEAM DIR   cancels this run's pending invitation, by its exact id
// env: ORIGIN (console), STATE (storage state of the team owner, read only, never rewritten), PW_CHROMIUM, [PW_MODULE],
// [MAIL_ORIGIN] the console the server writes into mail links (default ORIGIN). A link to any other host is never opened.
// DIR (mode 700) gets account.json and, once signed up, state.json in fresh_account.mjs's layout, so
// `fresh_account.mjs delete DIR` removes the account. Nothing secret is printed: no address, body, code or token.
import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { randomBytes, randomUUID } from "node:crypto"
import { newInbox, signUpForm, waitMessage } from "./console_signup.mjs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const mailOrigin = new URL(process.env.MAIL_ORIGIN || origin).origin
const [action, team, dir] = process.argv.slice(2)
if (!["run", "cleanup"].includes(action) || !team || !dir) {
  console.error("usage: console_invite_mail.mjs run|cleanup TEAM DIR")
  process.exit(2)
}
const write = (name, text) => (writeFileSync(`${dir}/${name}`, text, { mode: 0o600 }), chmodSync(`${dir}/${name}`, 0o600))
const read = (name) => (existsSync(`${dir}/${name}`) ? JSON.parse(readFileSync(`${dir}/${name}`, "utf8")) : null)
const SUBJECT = "Paperboat team invitation"
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
  return ok
}
const must = (ok, step, detail = "") => {
  if (!log(ok, step, detail)) throw new Error("stop")
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const owner = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const api = async (method, path, data) => {
  const reply = await owner.request.fetch(`${origin}/api/v1/${path}`, {
    method,
    headers: { origin, "content-type": "application/json", "idempotency-key": randomUUID() },
    data,
  })
  const body = await reply.json().catch(() => null)
  return { status: reply.status(), data: body?.data, code: body?.error?.code ?? body?.code }
}
const teamPath = `teams/${encodeURIComponent(team)}`
// A failed listing throws: it never reads as "no pending invitation".
const pending = async (email) => {
  const reply = await api("GET", `${teamPath}/invitations?state=pending&limit=200`)
  if (reply.status !== 200 || !Array.isArray(reply.data?.items)) throw new Error(`pending invitations not listed: HTTP ${reply.status} ${reply.code ?? ""}`)
  return reply.data.items.filter((i) => i.email === email)
}
// The row the server shows for the invitation once its outbox says the provider accepted the mail.
const sent = async (email, after = "") => {
  for (let i = 0; i < 30; i++) {
    const [row] = await pending(email)
    if (row?.delivery_state === "sent" && row.last_sent_at && row.last_sent_at > after) return row
    if (row?.delivery_state === "failed") return row
    await sleep(2000)
  }
  return (await pending(email))[0]
}
const teamLink = (mail) => {
  const text = `${mail.text ?? ""}\n${[mail.html].flat().join("\n")}`
  return (text.match(/https?:\/\/[^\s"'<>)\]]+/g) ?? []).map((u) => URL.parse(u)).find((u) => u?.pathname === "/team")
}

async function cleanup() {
  const account = read("account.json")
  if (!account) return console.log("nothing was invited")
  for (const row of await pending(account.email)) {
    const generation = (await api("GET", teamPath)).data?.generation
    const reply = await api("POST", `${teamPath}/invitations/${encodeURIComponent(row.invitation_id)}/cancel`, {
      operation_id: randomUUID(),
      expected_generation: generation,
    })
    if (reply.status >= 300) throw new Error(`invitation ${row.invitation_id} not cancelled: HTTP ${reply.status} ${reply.code ?? ""}`)
  }
  const left = await pending(account.email)
  if (left.length) throw new Error(`invitation ${left[0].invitation_id} is still pending`)
  console.log("no pending invitation of this run is left")
}

async function run() {
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const inbox = await newInbox("invite", randomBytes)
  write("account.json", JSON.stringify(inbox))
  const seen = new Set()
  const found = await api("GET", teamPath)
  must(found.status === 200 && found.data?.my_role === "owner", "the stored session owns the team", `HTTP ${found.status}`)

  const invitedAt = Date.now()
  const invited = await api("POST", `${teamPath}/invitations`, {
    operation_id: randomUUID(),
    expected_generation: found.data.generation,
    email: inbox.email,
  })
  must(invited.status < 300, "the owner invites a new address without a Paperboat account", `HTTP ${invited.status} ${invited.code ?? ""}`)
  const first = await waitMessage(inbox.mailToken, seen, (m) => m.subject === SUBJECT)
  must(Boolean(first), "the invitation email arrives", first ? `after ${Math.round((Date.now() - invitedAt) / 1000)} s` : "none in 180 s")
  const link = teamLink(first)
  must(link?.origin === mailOrigin, "the email links to the configured console's Team page", link ? `${link.origin}${link.pathname}` : "no /team link")
  const row = await sent(inbox.email)
  log(row?.delivery_state === "sent", "the server says the provider accepted the initial mail", row?.delivery_state ?? "no row")

  // Resend from the console's Invitations table; the server allows one mail a minute.
  await sleep(Math.max(0, invitedAt + 65000 - Date.now()))
  await owner.addCookies([{ name: "pb-workspace", value: team, url: origin }])
  const page = await owner.newPage()
  await page.goto(`${origin}/team`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
  const tab = page.getByRole("button", { name: /^Invitations/ }).or(page.getByRole("radio", { name: /^Invitations/ })).or(page.getByRole("tab", { name: /^Invitations/ }))
  let resent = 0
  try {
    await tab.first().click({ timeout: 60000 })
    const mine = page.locator("tbody tr").filter({ hasText: inbox.email.split("@")[0] })
    await mine.first().waitFor({ timeout: 30000 })
    const answered = page.waitForResponse((r) => r.request().method() === "POST" && /\/invitations\/[^/]+\/resend$/.test(new URL(r.url()).pathname), { timeout: 30000 })
    await mine.first().getByRole("button", { name: "Resend" }).click()
    resent = (await answered).status()
  } catch (error) {
    console.error(String(error.message).split("\n")[0].replaceAll(inbox.email, "<invitee>"))
  }
  if (log(resent === 200, "Resend in the console's Invitations table is accepted", `HTTP ${resent || "no request"}`)) {
    const second = await waitMessage(inbox.mailToken, seen, (m) => m.subject === SUBJECT)
    log(Boolean(second), "the resent email arrives")
    log(Boolean(second) && teamLink(second)?.href === link.href, "the resent email has the same link")
    const again = await sent(inbox.email, row?.last_sent_at ?? "")
    log(again?.delivery_state === "sent" && (again.last_sent_at ?? "") > (row?.last_sent_at ?? ""), "the server says the provider accepted the resent mail", again?.delivery_state ?? "no row")
  }

  // The invited person: a browser with no session opens the link from the email, signs up with that address.
  const guest = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  const mailPage = await guest.newPage()
  await mailPage.goto(link.href, { timeout: 120000 })
  const asked = await mailPage.getByRole("link", { name: "Sign up" }).waitFor({ timeout: 120000 }).then(() => true, () => false)
  must(asked, "the link asks a signed-out visitor to sign in", new URL(mailPage.url()).host)
  // From here the account may exist: with a state file, cleanup deletes it (fresh_account.mjs signs in again when the state
  // holds no session) or reports that it could not.
  write("state.json", JSON.stringify(await guest.storageState()))
  await signUpForm(mailPage, inbox, seen)
  await mailPage.waitForURL((u) => u.origin === mailOrigin && !u.pathname.startsWith("/auth/"), { waitUntil: "domcontentloaded", timeout: 420000 })
  write("state.json", JSON.stringify(await guest.storageState()))
  log(new URL(mailPage.url()).pathname === "/team", "after sign-up the console opens the Team page", new URL(mailPage.url()).pathname)
  if (new URL(mailPage.url()).pathname !== "/team") await mailPage.goto(`${mailOrigin}/team`, { timeout: 120000 })
  const card = await mailPage
    .getByText(new RegExp(`You.re invited to join\\s+${team}\\s+as\\s+member`))
    .first()
    .waitFor({ timeout: 90000 })
    .then(() => true, () => false)
  log(card, "the Team page shows the invitation to this team as member")
  log(card && (await mailPage.getByRole("button", { name: "Join team" }).count()) === 1, "the invitation offers Join team")
}

try {
  await (action === "run" ? run() : cleanup())
} catch (error) {
  if (error.message !== "stop") {
    failed++
    console.log(`FAIL  ${action} stopped  (${String(error.message).split("\n")[0].replace(/[\w.+-]+@[\w-]+\.[\w.]+/g, "<email>").slice(0, 200)})`)
  }
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
