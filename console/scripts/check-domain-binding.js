// Browser check for F8-02 (a tunnel hostname under a workspace domain: pick -> served with TLS ->
// remove). Paste into the console of a signed-in tab on a tunnel page (/tunnels/<id>, Personal
// workspace) whose service is up, with `window.__domain` set to a new hostname one level under a
// verified workspace domain (Domains page). The routing record for that one name is published in
// `window.__dnsRecords` ([{type, name, value}]) for a caller whose DNS cannot hold the domain's
// wildcard; the script waits for `window.__dnsReady = true`. Prints the steps (window.__checkSteps).
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

  const workspaceDomains = (await (await fetch("/api/v1/domains?workspace=personal")).json()).data?.items ?? []
  const parent = workspaceDomains.find((d) => d.state === "verified" && domain.endsWith(`.${d.hostname}`))
  pass("the hostname is under a verified workspace domain", Boolean(parent), parent?.hostname ?? "")
  if (!parent) return steps
  ;(await until(() => button("Add")))?.click() // the tunnel page renders Add after its first fetch
  const yours = await until(() => button("Your domain", dialog() ?? main()))
  yours?.click()
  const nameInput = await until(() => dialog()?.querySelector('input[aria-label="Name"]'))
  pass("Add a public hostname offers a name under your domains", Boolean(nameInput))
  if (!nameInput) return steps
  const domainMenu = dialog().querySelector('[aria-label="Domain"]')
  if (domainMenu) {
    domainMenu.click()
    ;(
      await until(() =>
        [...document.querySelectorAll("[role=menuitemradio]")].find((i) => i.textContent.trim() === parent.hostname),
      )
    )?.click()
    await sleep(300)
  }
  type(nameInput, domain.slice(0, -parent.hostname.length - 1))
  type(document.getElementById("hostname-service"), window.__port ?? "18088")
  await sleep(300)
  pass("no DNS step is asked for", !/Add record|DNS record/.test(dialog()?.innerText ?? ""))
  button("Add hostname", dialog())?.click()
  pass("hostname bound on the server", Boolean(await until(mine, 30000)))
  window.__dnsRecords = [
    { type: "CNAME", name: domain, value: parent.records.find((r) => r.purpose === "routing").value },
  ]
  await until(() => window.__dnsReady, 180000)
  const ready = await until(async () => (await mine())?.certificate?.state === "ready", 240000)
  pass("the domain's certificate is in place for the hostname", Boolean(ready))
  const final = await mine()
  pass(
    "hostname reports ready with a certificate expiry",
    final?.state === "ready" && Boolean(final?.certificate?.expires_at),
    `${final?.state} until ${final?.certificate?.expires_at ?? "?"}`,
  )
  // The page refreshes hostname states every 15 seconds.
  pass(
    "page shows the hostname as ready",
    Boolean(
      await until(
        () => !/Certificate failed|Getting a certificate/.test(section().split(domain)[1]?.slice(0, 60) ?? ""),
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
  pass("remove asks to confirm", Boolean(confirm) && /stops reaching this tunnel/.test(confirm.innerText))
  ;[...(confirm?.querySelectorAll("button") ?? [])].find((b) => /^Remove/.test(b.textContent.trim()))?.click()
  pass("binding removed on the server", Boolean(await until(async () => !(await mine()), 30000)))
  pass("hostname gone from the page", Boolean(await until(() => !section().includes(domain))))
  return steps
})()
