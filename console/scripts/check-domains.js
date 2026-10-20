// Browser check for F8-02 (custom domain: add -> DNS instructions -> verify -> TLS -> remove). Paste
// into the console of a signed-in tab on a tunnel page (/tunnels/<id>, Personal workspace) whose
// service is up, with `window.__domain` set to a hostname in a zone you control. Needs a caller that
// creates the two DNS records the page shows: the script publishes them in `window.__dnsRecords`
// ([{type, name, value}]) and waits for `window.__dnsReady = true`. It adds the hostname from the
// "Your domain" tab, reads the instructions, checks DNS until verified, waits for the certificate on
// the server, then removes the hostname and checks the binding is gone. Removed domains are held for
// a week, so use a new hostname each run. Prints the steps (also in window.__checkSteps).
;(async () => {
  const domain = window.__domain
  delete window.__domain
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
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
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const button = (label, root = main()) =>
    [...root.querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const section = () => {
    const text = main().innerText
    return text.slice(text.indexOf("Public hostnames"), text.indexOf("Danger zone")).replace(/\s+/g, " ")
  }
  const tunnelId = location.pathname.split("/").at(-1)
  // The console is reached over a tunnel that drops a request now and then; retry reads.
  const domains = async () => {
    for (let attempt = 0; ; attempt++) {
      try {
        return (await (await fetch(`/api/v1/tunnels/${tunnelId}/domains`)).json()).data.items
      } catch (error) {
        if (attempt >= 4) throw error
        await new Promise((resolve) => setTimeout(resolve, 600))
      }
    }
  }
  const mine = async () => (await domains()).find((d) => d.hostname === domain)

  pass("a test hostname was given", Boolean(domain), domain ?? "")
  if (!domain) return steps
  pass("hostname is not bound yet", !(await mine()))

  button("Add")?.click()
  const yours = await until(() => button("Your domain", dialog() ?? main()))
  yours?.click()
  await sleep(500)
  const hostInput = await until(() =>
    [...(dialog()?.querySelectorAll("input") ?? [])].find((i) => /example/.test(i.placeholder)),
  )
  pass("Add a public hostname offers Your domain", Boolean(hostInput))
  if (!hostInput) return steps
  type(hostInput, domain)
  type(document.getElementById("hostname-service"), window.__port ?? "18088")
  await sleep(300)
  button("Add hostname", dialog())?.click()
  pass("domain bound on the server", Boolean(await until(mine, 30000)))
  const records = await until(() => {
    const found = [...section().matchAll(/Add record (\S+) (\S+) → (\S+)/g)].map((m) => ({
      type: m[1],
      name: m[2],
      value: m[3],
    }))
    return found.length >= 2 ? found : null
  })
  pass(
    "page shows the DNS records to add",
    Boolean(records),
    records ? records.map((r) => `${r.type} ${r.name}`).join(" | ") : section(),
  )
  if (!records) return steps
  pass(
    "one record points the name at the tunnel and one answers the certificate challenge",
    records.some((r) => r.name === domain) && records.some((r) => r.name === `_acme-challenge.${domain}`),
  )
  window.__dnsRecords = records
  await until(() => window.__dnsReady, 180000)
  const verified = await until(async () => {
    button("Check again")?.click()
    await sleep(2000)
    const d = await mine()
    return d && d.dns?.observed_records?.length && !/Waiting for DNS/.test(section()) ? d : null
  }, 120000)
  pass("DNS verified from the console", Boolean(verified))
  const ready = await until(async () => (await mine())?.certificate?.state === "ready", 240000)
  pass("certificate issued", Boolean(ready))
  const final = await mine()
  pass(
    "domain reports ready with a certificate expiry",
    final?.state === "ready" && Boolean(final?.certificate?.expires_at),
    `${final?.state} until ${final?.certificate?.expires_at ?? "?"}`,
  )
  // The page refreshes domain states every 15 seconds.
  pass(
    "page shows the hostname as ready",
    Boolean(
      await until(
        () =>
          !/Certificate failed|Waiting for DNS|Getting a certificate/.test(
            section().split(domain)[1]?.slice(0, 60) ?? "",
          ),
        45000,
      ),
    ),
    section().split(domain)[1]?.slice(0, 40) ?? "",
  )
  window.__domainReady = true
  await until(() => window.__removeNow, 120000)

  const actions = [...main().querySelectorAll("button")].find(
    (b) => b.getAttribute("aria-label") === `Actions for ${domain}`,
  )
  actions?.click()
  const remove = await until(() =>
    [...document.querySelectorAll("[role=menuitem]")].find((i) => /Remove hostname/.test(i.textContent)),
  )
  remove?.click()
  const confirm = await until(() => dialog())
  pass(
    "remove asks to confirm and says the DNS record stays",
    Boolean(confirm) && /DNS record stays/.test(confirm.innerText),
  )
  ;[...(confirm?.querySelectorAll("button") ?? [])].find((b) => /^Remove/.test(b.textContent.trim()))?.click()
  pass("binding removed on the server", Boolean(await until(async () => !(await mine()), 30000)))
  pass("hostname gone from the page", Boolean(await until(() => !section().includes(domain))))
  return steps
})()
