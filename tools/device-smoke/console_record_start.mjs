// Starts a recording of one CLI terminal session from the live console, as the session's owner: the web terminal's
// "Start recording" control (console/components/terminal/terminal-page.tsx), the only place a recording starts.
// usage: node console_record_start.mjs --team TEAM --session SESSION_ID [--print-plan]
//   (the interface recordings_key_rotation.py --console-start-cmd / SMOKE_RECKEY_CONSOLE_START_CMD runs)
// env: ORIGIN (console origin), STATE (owner's storage state from console_session.mjs, mode 600, never printed),
//      PW_CHROMIUM (headless Chromium), [PW_MODULE] (playwright-core index.mjs), [SMOKE_RECKEY_START_TIMEOUT_MS] (default 90000).
// Exit 0 once the page shows the recording active; exit 1 with one reason line on stderr; exit 2 on bad arguments.
// The terminal-session id the `pb session` commands print is the console's /terminals/<id>. Recording needs the team's
// recording key to exist already: the console only creates it under Team > Recordings (Set up, an admin with an ENV vault),
// not at the first start, so without a key the start is refused (recording_key_unavailable) and this exits 1 with that sentence.
// No vault password is needed to start a recording, so none is read.
const SEL = {
  attached: "[data-terminal-phase=attached]", // terminal-surface.tsx:64
  start: { role: "button", name: "Start recording" }, // terminal-page.tsx:213 (aria-label, shown to the owner only)
  stop: { role: "button", name: "Stop recording" }, // terminal-page.tsx:213, replaces Start once recording
  active: "[data-terminal-recording]", // terminal-surface.tsx:295 (the REC chip)
  refusal: "[data-sonner-toast]", // terminal-page.tsx:220 toast.error(recordingSentences[code])
}
const arg = (name) => { const i = process.argv.indexOf(`--${name}`); return i > 0 ? process.argv[i + 1] : undefined }
const team = arg("team")
const session = arg("session")
if (!team || !session || /[^A-Za-z0-9_.-]/.test(session)) {
  console.error("usage: console_record_start.mjs --team TEAM --session SESSION_ID [--print-plan] (session id: letters, digits, _ . -)")
  process.exit(2)
}
const steps = [
  "open ORIGIN/terminals/SESSION_ID with the owner STATE",
  `wait for ${SEL.attached} (the terminal is attached)`,
  `click button "Start recording"; no dialog or consent is shown by the console on this path`,
  `wait for ${SEL.active} and button "Stop recording" (recording active); a ${SEL.refusal} appearing instead is the refusal reason`,
]
if (process.argv.includes("--print-plan")) {
  console.log(JSON.stringify({ team, session, steps, selectors: SEL }, null, 1))
  process.exit(0)
}
const fail = (why) => { console.error(`console record start failed: ${why}`); process.exit(1) }
for (const name of ["ORIGIN", "STATE", "PW_CHROMIUM"]) if (!process.env[name]) fail(`set ${name}`)
const timeout = Number(process.env.SMOKE_RECKEY_START_TIMEOUT_MS ?? 90000)
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
try {
  const context = await browser.newContext({ storageState: process.env.STATE })
  const page = await context.newPage()
  await page.goto(`${process.env.ORIGIN}/terminals/${encodeURIComponent(session)}`, { timeout })
  if (/\/(sign-?in|auth|login)/.test(new URL(page.url()).pathname)) fail("the console session in STATE is signed out")
  await page.locator(SEL.attached).waitFor({ timeout }).catch(() => fail("the terminal never attached (missing, closed or not this account's)"))
  const start = page.getByRole(SEL.start.role, { name: SEL.start.name, exact: true })
  if (!(await start.count())) fail(`no Start recording control: ${(await page.getByRole("button", { name: SEL.stop.name, exact: true }).count()) ? "already recording" : "STATE is not the session's owner"}`)
  await start.click()
  const done = page.locator(SEL.active).waitFor({ timeout: 30000 }).then(() => "")
  const refused = page.locator(SEL.refusal).first().waitFor({ timeout: 30000 }).then(async () => (await page.locator(SEL.refusal).first().innerText()).replace(/\s+/g, " "))
  const refusal = await Promise.race([done, refused]).catch(() => fail("the page showed neither the REC chip nor a refusal"))
  if (refusal) fail(`the console refused: ${refusal}`)
  await page.getByRole(SEL.stop.role, { name: SEL.stop.name, exact: true }).waitFor({ timeout: 10000 }).catch(() => fail("REC chip shown but the control did not become Stop recording"))
  console.log("recording started")
} catch (error) {
  fail(String(error?.message ?? error).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
