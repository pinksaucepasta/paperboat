// Browser check for F8-08. Paste into the console of a signed-in tab on /tunnels (Personal
// workspace) after creating a tunnel on a machine with its local service up:
//   pb tunnel create lanec-f808 --port 18088     (any name/port; set window.__tunnel / __port)
// Lists the tunnel and compares the page with `GET /v1/tunnels`, shows the "New tunnel" command,
// opens the tunnel page (status, address, machine), renames and restores it, pauses and resumes
// it, adds a public hostname route with a path, edits its service, turns it off, removes it, and
// checks the server after every step. Leaves the tunnel as it found it. Prints the steps.
;(async () => {
  const name = window.__tunnel ?? "lanec-f808"
  const port = window.__port ?? "18088"
  delete window.__tunnel
  delete window.__port
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
    for (let waited = 0; waited < ms; waited += 250) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const text = () => main().innerText
  const buttons = (root = main()) => [...root.querySelectorAll("button")]
  const button = (label, root = main()) => buttons(root).find((b) => b.textContent.trim() === label)
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const closeDialog = async () => {
    const d = dialog()
    ;(d && (button("Cancel", d) ?? button("Close", d)))?.click()
    await sleep(500)
  }
  // The console is reached over a tunnel that drops a request now and then; retry reads.
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      try {
        return (await (await fetch(`/api/v1/${path}`)).json()).data
      } catch (error) {
        if (attempt >= 4) throw error
        await new Promise((resolve) => setTimeout(resolve, 600))
      }
    }
  }
  const tunnels = async () =>
    (await get("tunnels?workspace=personal")).items.filter((t) => t.desired_state !== "deleted")

  // List.
  const known = await tunnels()
  const mine = known.find((tunnel) => tunnel.name === name)
  pass("tunnel exists on the server", Boolean(mine), name)
  if (!mine) return steps
  const row = await until(() => [...main().querySelectorAll("a")].find((a) => a.textContent.trim() === name))
  pass("list shows the tunnel", Boolean(row))
  pass(
    "list rows match the server",
    known.every((t) => text().includes(t.name)),
    `${known.length} tunnels`,
  )
  const listed = row?.closest("tr")?.innerText ?? ""
  const host = new URL(mine.stable_endpoint).hostname
  pass("row shows its address", listed.includes(host))

  // New tunnel: the exact command, nothing created.
  button("New tunnel")?.click()
  const create = await until(() => dialog())
  pass(
    "New tunnel shows the pb tunnel create command",
    Boolean(create) && /pb tunnel create \S+ --port \d+/.test(create.innerText),
    create?.innerText.replace(/\s+/g, " ").slice(0, 120) ?? "",
  )
  await closeDialog()
  pass("nothing was created", (await tunnels()).length === known.length)

  // Tunnel page.
  row.click()
  // The page draws its sections before its heading has the tunnel's name: wait for both.
  const opened = await until(() => (text().includes("Danger zone") && text().startsWith(name) ? text() : null))
  pass("tunnel page opens", Boolean(opened))
  const healthy = await until(() => /Healthy/.test(text()), 30000)
  pass("status is Healthy", Boolean(healthy))
  pass(
    "page shows address and machine link",
    text().includes(host) &&
      Boolean(
        [...main().querySelectorAll("a")].find((a) => /\/machines\/.+\/tunnels/.test(a.getAttribute("href") ?? "")),
      ),
  )
  const base = `tunnels/${mine.id}`
  const path = `/check-${Date.now().toString(36)}.html`

  // Rename (in place on the title; the address stays) and restore.
  const rename = async (to) => {
    document.querySelector('button[aria-label="Rename"]')?.click()
    const input = await until(() => document.querySelector('input[aria-label="Name"]'))
    if (!input) return false
    type(input, to)
    await sleep(200)
    input.form.requestSubmit()
    const saved = Boolean(await until(async () => (await get(base)).name === to))
    // The page leaves edit mode once it has refetched; a second rename started before that edits stale state.
    const settled = await until(
      () => !document.querySelector('input[aria-label="Name"]') && document.querySelector("h1")?.textContent === to,
    )
    return saved && Boolean(settled)
  }
  pass("rename saved on the server", await rename(`${name}-renamed`))
  pass("the address did not change with the name", (await get(base)).stable_endpoint === mine.stable_endpoint)
  pass("name restored", await rename(name))

  // Pause and resume. Pausing needs no confirmation; a click that lands while the page re-renders
  // is repeated.
  let paused = false
  for (let attempt = 0; attempt < 3 && !paused; attempt++) {
    button("Pause")?.click()
    const confirmPause = await until(() => dialog(), 1500)
    if (confirmPause) (button("Pause", confirmPause) ?? button("Pause tunnel", confirmPause))?.click()
    paused = Boolean(await until(async () => (await get(base)).desired_state === "paused", 6000))
  }
  pass("paused on the server", paused)
  let resumed = false
  for (let attempt = 0; attempt < 3 && !resumed; attempt++) {
    const resume = await until(() => button("Resume") ?? button("Resume tunnel"), 3000)
    resume?.click()
    const confirmResume = await until(() => dialog(), 1500)
    if (confirmResume) (button("Resume", confirmResume) ?? button("Resume tunnel", confirmResume))?.click()
    resumed = Boolean(await until(async () => (await get(base)).desired_state === "active", 6000))
  }
  pass("resumed on the server", resumed)
  pass("healthy again after resume", Boolean(await until(() => /Healthy/.test(text()), 120000)))

  // Public hostname route with a path.
  const routesBefore = (await get(`${base}/routes`)).items.length
  // The route this run adds is the one that was not listed before it: routes left by earlier runs do not count.
  const knownRoutes = new Set((await get(`${base}/routes`)).items.map((r) => r.id))
  const addedRoute = async () => (await get(`${base}/routes`)).items.find((r) => !knownRoutes.has(r.id))
  const section = () => text().slice(text().indexOf("Public hostnames"), text().indexOf("Danger zone"))
  button("Add")?.click()
  const pathInput = await until(() => document.getElementById("hostname-path"))
  if (pathInput) {
    type(pathInput, path)
    type(document.getElementById("hostname-service"), port)
    await sleep(300)
    button("Add hostname", dialog())?.click()
    pass(
      "route added on the server",
      Boolean(await until(async () => (await get(`${base}/routes`)).items.length === routesBefore + 1)),
    )
    pass("route listed on the page", Boolean(await until(() => section().includes(`${host}${path}`))))
    const rowActions = () => buttons().find((b) => b.getAttribute("aria-label") === `Actions for ${host}${path}`)
    const menuItem = async (label) => {
      rowActions()?.click()
      const item = await until(() =>
        [...document.querySelectorAll("[role=menuitem]")].find((i) => i.textContent.trim().startsWith(label)),
      )
      item?.click()
      return Boolean(item)
    }
    // Edit the service.
    pass("Edit… opens", await menuItem("Edit"))
    const service = await until(() => document.getElementById("hostname-service"))
    if (service) {
      type(service, `127.0.0.1:${Number(port) + 1}`)
      await sleep(200)
      button("Save", dialog())?.click()
      const edited = await until(async () => (await addedRoute())?.origin.address === `127.0.0.1:${Number(port) + 1}`)
      pass("service edit saved on the server", Boolean(edited))
    }
    // Turn off, then remove.
    pass("Turn off opens", await menuItem("Turn off"))
    const off = await until(() => dialog(), 1500)
    if (off) button("Turn off", off)?.click()
    pass(
      "route turned off on the server",
      Boolean(
        await until(async () => {
          const added = await addedRoute()
          return added && added.desired_state !== "active"
        }),
      ),
    )
    // A click that lands while the page re-renders is repeated, as for Pause. A deleted route disappears from the list.
    let opened = false
    let removed = false
    for (let attempt = 0; attempt < 3 && !removed; attempt++) {
      opened = (await menuItem("Remove hostname")) || opened
      const confirmRemove = await until(() => dialog(), 3000)
      if (confirmRemove)
        buttons(confirmRemove)
          .find((b) => /^Remove/.test(b.textContent.trim()))
          ?.click()
      removed = Boolean(await until(async () => !(await addedRoute()), 6000))
    }
    pass("Remove hostname opens", opened)
    pass("route removed on the server", removed)
    pass("route gone from the page", Boolean(await until(() => !section().includes(`${host}${path}`))))
    // The same hostname and path can be added again after removal.
    button("Add")?.click()
    const again = await until(() => document.getElementById("hostname-path"))
    if (again) {
      type(again, path)
      type(document.getElementById("hostname-service"), port)
      await sleep(300)
      button("Add hostname", dialog())?.click()
      pass(
        "same route can be added again after removal",
        Boolean(await until(() => section().includes(`${host}${path}`))),
        dialog()?.innerText.replace(/\s+/g, " ").slice(-80) ?? "",
      )
      await closeDialog()
      const readded = buttons().find((b) => b.getAttribute("aria-label") === `Actions for ${host}${path}`)
      if (readded) {
        readded.click()
        const remove = await until(() =>
          [...document.querySelectorAll("[role=menuitem]")].find((i) =>
            i.textContent.trim().startsWith("Remove hostname"),
          ),
        )
        remove?.click()
        const sure = await until(() => dialog())
        if (sure)
          buttons(sure)
            .find((b) => /^Remove/.test(b.textContent.trim()))
            ?.click()
        pass("re-added route removed", Boolean(await until(() => !section().includes(`${host}${path}`))))
      }
    }
  } else {
    pass("Add a public hostname opens", false)
  }
  pass("tunnel still active at the end", (await get(base)).desired_state === "active")
  return steps
})()
