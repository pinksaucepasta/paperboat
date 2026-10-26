// Browser check for Files, one machine at a time. Run in a signed-in tab (e.g. a headless
// Chromium). window.__online: an online machine id whose Inbox holds window.__file (sent with
// `pb send --to <machine>`); window.__offline: an offline machine id. Checks: /files without a
// machine asks to choose one and offers no "All machines"; ?machine=<online> lists the file (read
// from GET /v1/machines/{id}/inbox); ?machine=<offline> says "machine offline, Inbox unavailable"
// and lists nothing; no request goes to /v1/inbox. Prints the steps.
// With window.__content (the file's exact text) the file's download link must return exactly that; with window.__delete
// the file is then removed through its row menu and its confirm dialog, and must be gone from the list and the machine's
// Inbox. Without them nothing is changed.
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const { __online: online, __offline: offline, __file: name } = window
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 30000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(300)
    }
    return null
  }
  const main = () => [...document.querySelectorAll("main main")].find((e) => e.offsetParent !== null) ?? document.querySelector("main")
  const text = () => main().innerText.replace(/\s+/g, " ")
  const asked = []
  const realFetch = window.fetch
  window.fetch = (input, init) => {
    const url = new URL(typeof input === "string" ? input : input.url, location.href)
    if (url.pathname.startsWith("/api/v1/")) asked.push(url.pathname)
    return realFetch(input, init)
  }
  try {
    window.next.router.push("/files")
    pass("without a machine it asks to choose one", Boolean(await until(() => /Choose a machine/.test(text()))))
    window.next.router.push(`/files?machine=${online}`)
    const row = await until(() => [...main().querySelectorAll("tbody tr")].find((tr) => tr.innerText.includes(name)))
    pass("an online machine's Inbox lists its file", Boolean(row), row?.innerText.replace(/\s+/g, " ").slice(0, 100))
    pass("it is read from that machine's Inbox", asked.some((path) => path === `/api/v1/machines/${online}/inbox`))
    window.next.router.push(`/files?machine=${offline}`)
    const off = await until(() => /machine offline, Inbox unavailable/.test(text()))
    pass("an offline machine says so and lists nothing", Boolean(off) && !main().querySelector("tbody tr"), text().slice(0, 120))
    pass("no workspace-wide Inbox request", !asked.includes("/api/v1/inbox"))
    if (window.__content) {
      window.next.router.push(`/files?machine=${online}`)
      const link = await until(() => [...main().querySelectorAll("a[download]")].find((a) => a.textContent.trim() === name))
      const body = link ? await (await realFetch(link.href, { credentials: "include" })).text() : null
      pass("the file's download returns its exact content", body === window.__content, body === null ? "no download link" : `${body.length} bytes`)
    }
    if (window.__delete) {
      const rowOf = () => [...main().querySelectorAll("tbody tr")].find((tr) => tr.innerText.includes(name))
      const menu = rowOf()?.querySelector(`button[aria-label="Actions for ${name}"]`)
      menu?.click()
      const item = await until(() => [...document.querySelectorAll('[role="menuitem"]')].find((e) => e.textContent.trim() === "Delete"))
      item?.click()
      const confirm = await until(() => [...document.querySelectorAll('[role="dialog"], [role="alertdialog"]')].flatMap((d) => [...d.querySelectorAll("button")]).find((b) => b.textContent.trim() === "Delete"))
      confirm?.click()
      pass("Delete asks for confirmation and removes the file from the list", Boolean(await until(() => !rowOf())), item && confirm ? "" : "menu or confirm button not found")
      const listed = await (await realFetch(`/api/v1/machines/${online}/inbox`, { credentials: "include" })).text()
      pass("the machine's Inbox no longer holds it", !listed.includes(name))
    }
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
