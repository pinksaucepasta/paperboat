// Pays a Stripe SANDBOX Checkout page with the test card 4242 4242 4242 4242, as a hosted-checkout buyer: the way a throwaway team is made
// by the real product path (`pb team create` prints the Checkout link; the server makes the team when Stripe reports the payment).
// usage: PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node stripe_checkout.mjs <checkout url>
// Exit 0 once Checkout leaves the payment page (the success redirect); exit 1 with one reason line on stderr.
import { payTestCheckout, testCheckoutURL } from "./stripe_pay.mjs"

const url = process.argv[2]
if (!testCheckoutURL(url)) {
  console.error("usage: stripe_checkout.mjs https://checkout.stripe.com/.../pay/cs_test_... (a sandbox Checkout link)")
  process.exit(2)
}
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
try {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 1400 } })).newPage()
  await page.goto(url, { waitUntil: "domcontentloaded", timeout: 60000 })
  await payTestCheckout(page)
  console.log("paid")
} catch (error) {
  console.error(`stripe checkout failed: ${String(error?.message ?? error).split("\n")[0].slice(0, 200)}`)
  process.exitCode = 1
} finally {
  await browser.close()
}
