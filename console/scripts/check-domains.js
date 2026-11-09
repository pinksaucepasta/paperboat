// Browser check for the Domains page (F8-02): add a workspace domain, show its DNS records, verify it
// live, show the certificate, refuse nothing extra, remove it. Paste into a signed-in tab on /domains with
// `window.__domain` set to a new name in a zone you control (e.g. wsd-<n>.<test zone>). The caller
// creates the records the script publishes in `window.__dnsRecords` ([{type, name, value, purpose}];
// the verification CNAME is enough) and sets `window.__dnsReady = true`. Prints the steps
// (window.__checkSteps).
;(async () => {
  const domain = window.__domain
  delete window.__domain
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    for (let waited = 0; waited < ms; waited += 500) {
      const found = await test()
      if (found) return found
      await sleep(500)
    }
    return null
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const button = (label, root = document) =>
    [...root.querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const panel = () => document.querySelector('[data-slot="sheet-content"]')
  const workspace = new URLSearchParams(location.search).get("workspace") ?? "personal"
  const listed = async () =>
    (await (await fetch(`/api/v1/domains?workspace=${encodeURIComponent(workspace)}`)).json()).data?.items ?? []

  const add = await until(() => button("Add domain"))
  pass("the page offers Add domain", Boolean(add))
  add.click()
  const input = await until(() => document.querySelector("#domain-name"))
  type(input, domain)
  await sleep(200)
  button("Add domain", document.querySelector("[role=dialog]")).click()
  const opened = await until(() => panel()?.innerText.includes(domain) && panel())
  pass("adding opens the domain's panel", Boolean(opened))
  const text = opened?.innerText ?? ""
  pass(
    "the panel shows the verification and routing records",
    text.includes(`_acme-challenge.${domain}`) && text.includes(`*.${domain}`) && text.includes("Waiting for DNS"),
  )
  const record = (await listed()).find((item) => item.hostname === domain)
  window.__dnsRecords = record?.records ?? []
  pass("the server lists the domain as waiting for DNS", record?.state === "pending_dns")
  await until(() => window.__dnsReady === true, 600000)
  button("Check now", panel())?.click()
  pass(
    "verification shows live",
    Boolean(await until(() => /Getting a certificate|Ready/.test(panel()?.innerText ?? ""), 300000)),
  )
  pass(
    "the certificate covers the domain and every name under it",
    Boolean(await until(() => panel()?.innerText.includes("Covers"), 300000)),
  )
  button("Remove", panel()).click()
  const confirm = await until(() => document.querySelector("[role=alertdialog]"))
  button("Remove domain", confirm).click()
  pass(
    "Remove deletes it",
    Boolean(await until(async () => !(await listed()).some((item) => item.hostname === domain))),
  )
  return steps
})()
