// Pays a Stripe SANDBOX Checkout page with the test card 4242 4242 4242 4242, as a hosted-checkout buyer: the way a throwaway team is made
// by the real product path (`pb team create` prints the Checkout link; the server makes the team when Stripe reports the payment).
// usage: PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node stripe_checkout.mjs <checkout url>
// Exit 0 once Checkout leaves the payment page (the success redirect); exit 1 with one reason line on stderr.
const url = process.argv[2]
if (!/^https:\/\/checkout\.stripe\.com\//.test(url ?? "")) {
  console.error("usage: stripe_checkout.mjs https://checkout.stripe.com/... (a sandbox Checkout link)")
  process.exit(2)
}
const fail = (why) => { console.error(`stripe checkout failed: ${why}`); process.exit(1) }
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
try {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 1400 } })).newPage()
  await page.goto(url, { waitUntil: "domcontentloaded", timeout: 60000 })
  if (!(await page.getByText("Sandbox", { exact: true }).first().waitFor({ timeout: 60000 }).then(() => true, () => false))) fail("not a sandbox Checkout page")
  // The form sits in Stripe's frames: take the first frame (the page itself included) where the locator finds something.
  const find = async (what, pick) => {
    for (let i = 0; i < 60; i++) {
      for (const frame of page.frames()) {
        const locator = pick(frame)
        if (await locator.count()) return locator.first()
      }
      await page.waitForTimeout(500)
    }
    fail(`no ${what} on the Checkout page`)
  }
  const field = (placeholder) => find(`"${placeholder}" field`, (f) => f.getByPlaceholder(placeholder, { exact: true }))
  await (await field("Card number")).fill("4242424242424242")
  await (await field("MM / YY")).fill("1234")
  await (await field("Security code")).fill("123")
  await (await field("First and last name")).fill("Paperboat Smoke")
  const terms = await find("terms checkbox", (f) => f.getByRole("checkbox", { name: /Start the service now/ }))
  // Stripe's consent box (#paymentForm-termsOfServiceConsentInput) is a React-controlled input: right after the click its
  // DOM state can still read unchecked, so locator.check() reported "Clicking the checkbox did not change its state" for a
  // box that read checked a moment later (1 of 6 runs, 2026-11-27). Click once, then wait for the state; fail only if it never turns checked.
  await terms.click()
  for (let i = 0; !(await terms.isChecked()); i++) {
    if (i >= 50) fail("the terms checkbox did not turn checked within 5 s")
    await page.waitForTimeout(100)
  }
  await (await find("Subscribe button", (f) => f.getByRole("button", { name: /^(Subscribe|Pay)/ }))).click()
  await page.waitForURL((u) => u.hostname !== "checkout.stripe.com", { timeout: 120000 }).catch(() => fail("Checkout did not accept the payment: " + page.url().slice(0, 40)))
  console.log("paid")
} catch (error) {
  fail(String(error?.message ?? error).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
