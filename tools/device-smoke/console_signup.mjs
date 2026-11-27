// The disposable inbox (mail.tm, an address this test setup creates and alone can read) and the AuthKit sign-up form,
// shared by fresh_account.mjs and console_invite_mail.mjs. Nothing secret is printed.

// mail.tm answers 429 when several runs share this address: wait as it asks (Retry-After, else 5 s) and retry, at most 8 times
export const mailtm = async (path, token, body) => {
  for (let attempt = 0; ; attempt++) {
    const r = await fetch("https://api.mail.tm" + path, {
      method: body ? "POST" : "GET",
      headers: { "content-type": "application/json", ...(token ? { authorization: `Bearer ${token}` } : {}) },
      body: body && JSON.stringify(body),
    })
    if (r.status === 429 && attempt < 8) {
      await new Promise((resolve) => setTimeout(resolve, Math.min(30, Number(r.headers.get("retry-after")) || 5) * 1000))
      continue
    }
    if (!r.ok) throw new Error(`mail.tm ${path.replace(/\/messages\/.*/, "/messages/<id>")}: HTTP ${r.status}`)
    return r.json()
  }
}

/** A new inbox: {email, password (for the console account), mailToken}. The caller keeps it in a mode-600 file. */
export async function newInbox(prefix, random) {
  const domain = (await mailtm("/domains"))["hydra:member"][0].domain
  const email = `${prefix}-${random(5).toString("hex")}@${domain}`
  const password = random(12).toString("hex") + "Aa1!"
  await mailtm("/accounts", null, { address: email, password })
  return { email, password, mailToken: (await mailtm("/token", null, { address: email, password })).token }
}

/** The first message not in `seen` that `wanted` accepts (called with the full message), within `ms`; it is added to `seen`. */
export async function waitMessage(mailToken, seen, wanted, ms = 180000) {
  const end = Date.now() + ms
  while (Date.now() < end) {
    for (const summary of (await mailtm("/messages", mailToken))["hydra:member"] ?? []) {
      if (seen.has(summary.id)) continue
      const message = await mailtm(`/messages/${summary.id}`, mailToken)
      if (!wanted(message)) continue
      seen.add(summary.id)
      return message
    }
    await new Promise((resolve) => setTimeout(resolve, 4000))
  }
  return null
}

const sixDigits = (message) => (message.text ?? "").match(/\b(\d{6})\b/)?.[1]

/** From an AuthKit sign-in page: "Sign up", the form, and the emailed code. The caller waits for the redirect back. */
export async function signUpForm(page, { email, password, mailToken }, seen = new Set()) {
  await page.getByRole("link", { name: "Sign up" }).click()
  await page.waitForTimeout(3000)
  for (const [selector, value] of [['input[name="email"]', email], ['input[name="first_name"]', "Final"], ['input[name="last_name"]', "Recheck"]]) {
    if (await page.locator(selector).count()) await page.fill(selector, value)
  }
  await page.keyboard.press("Enter")
  await page.waitForTimeout(4000)
  if (await page.locator('input[type="password"]').count()) {
    await page.fill('input[type="password"]', password)
    await page.keyboard.press("Enter")
    await page.waitForTimeout(5000)
  }
  const mail = await waitMessage(mailToken, seen, sixDigits, 150000)
  if (!mail) throw new Error("no verification code arrived in the disposable inbox")
  const code = sixDigits(mail)
  const boxes = page.locator('input[inputmode="numeric"], input[name="code"], input[autocomplete="one-time-code"]')
  if ((await boxes.count()) > 1) (await boxes.first().click(), await page.keyboard.type(code))
  else await boxes.first().fill(code)
  await page.keyboard.press("Enter").catch(() => {})
}
