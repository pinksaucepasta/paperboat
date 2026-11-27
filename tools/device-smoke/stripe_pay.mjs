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
  await (await field("Card number")).fill(card)
  await (await field("MM / YY")).fill("1234")
  await (await field("Security code")).fill("123")
  await (await field("First and last name")).fill("Paperboat Smoke")
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
  await page.waitForURL((u) => u.hostname !== "checkout.stripe.com", { timeout: 120000, waitUntil: "commit" }).catch(() => {
    throw new Error("Checkout did not accept the payment: " + page.url().slice(0, 40))
  })
}
