// The Stripe TEST-MODE Checkout page as a buyer, shared by stripe_checkout.mjs and billing_console.mjs.
// Nothing is typed unless the page is a test one: a cs_test_ session on checkout.stripe.com AND the "Sandbox" badge.
export const testCheckoutURL = (url) => /^https:\/\/checkout\.stripe\.com\/[a-z]+\/pay\/cs_test_[A-Za-z0-9]+/.test(url ?? "")

// Pays the Checkout page that `page` shows with the test card 4242 4242 4242 4242; resolves once Checkout leaves the payment page.
export async function payTestCheckout(page, card = "4242424242424242") {
  if (!testCheckoutURL(page.url())) throw new Error("refusing to pay: not a Stripe test-mode (cs_test_) Checkout session")
  if (!(await page.getByText("Sandbox", { exact: true }).first().waitFor({ timeout: 60000 }).then(() => true, () => false)))
    throw new Error("refusing to pay: not a sandbox Checkout page")
  // The form sits in Stripe's frames: take the first frame (the page itself included) where the locator finds something.
  const find = async (what, pick) => {
    for (let i = 0; i < 60; i++) {
      for (const frame of page.frames()) {
        const locator = pick(frame)
        if (await locator.count()) return locator.first()
      }
      await page.waitForTimeout(500)
    }
    throw new Error(`no ${what} on the Checkout page`)
  }
  const field = (placeholder) => find(`"${placeholder}" field`, (f) => f.getByPlaceholder(placeholder, { exact: true }))
  const values = [["Card number", card], ["MM / YY", "1234"], ["Security code", "123"], ["First and last name", "Paperboat Smoke"]]
  // Stripe's masked inputs can drop a value written while the form is still settling ("Your card number is incomplete" on submit,
  // 2026-11-27): type each field key by key, then submit only after every field has kept its value on two reads a second apart.
  await keepTyped(page, async (name) => await field(name), values)
  const terms = await find("terms checkbox", (f) => f.getByRole("checkbox", { name: /Start the service now/ }))
  // Stripe's consent box is a React-controlled input: right after the click its DOM state can still read unchecked, so
  // locator.check() reported "did not change its state" for a box that read checked a moment later (1 of 6 runs,
  // 2026-11-27). Click once, then wait for the state; fail only if it never turns checked.
  await terms.click()
  for (let i = 0; !(await terms.isChecked()); i++) {
    if (i >= 50) throw new Error("the terms checkbox did not turn checked within 5 s")
    await page.waitForTimeout(100)
  }
  await (await find("Subscribe button", (f) => f.getByRole("button", { name: /^(Subscribe|Pay)/ }))).click()
  await page.waitForURL((u) => u.hostname !== "checkout.stripe.com", { timeout: 120000, waitUntil: "commit" }).catch(async () => {
    throw new Error("Checkout did not accept the payment: " + (await whyStillThere(page)))
  })
}

// A masked input shows "4242 4242 4242 4242" or "12 / 34": spaces and slashes are the mask's.
export const sameValue = (shown, wanted) => shown.replace(/[\s/]/g, "") === wanted.replace(/[\s/]/g, "")

export async function keepTyped(page, field, values) {
  let good = 0
  for (let round = 0; round < 8 && good < 2; round++) {
    let wrong = 0
    for (const [name, value] of values) {
      const input = await field(name)
      if (sameValue(await input.inputValue(), value)) continue
      wrong++
      await input.fill("")
      await input.pressSequentially(value, { delay: 40 })
    }
    good = wrong ? 0 : good + 1
    await page.waitForTimeout(1000)
  }
  if (good < 2) throw new Error("the payment fields did not keep what was typed into them")
}

// What a Checkout page that did not move on shows: a human-verification challenge, its own error lines, or the pay button's label.
export async function whyStillThere(page) {
  const challenge = page.frames().some((f) => /hcaptcha|recaptcha|challenge/i.test(f.url()) && !/invisible|checkbox-invisible/i.test(f.url()))
  const said = []
  for (const frame of page.frames()) {
    for (const text of await frame.locator('[role="alert"], .FieldError, .ConfirmPaymentButton-ErrorText, [data-testid="submit-button-processing-label"]').allInnerTexts().catch(() => [])) if (text.trim()) said.push(text.trim())
  }
  const visible = await page.getByText(/select all|verify you are human|I am human|something went wrong|try again|declined|processing/i).allInnerTexts().catch(() => [])
  return `${challenge ? "a challenge frame is present; " : ""}page says: ${[...new Set([...said, ...visible.map((t) => t.trim())])].join(" / ").replace(/\s+/g, " ").slice(0, 120) || "nothing"}`
}
