// Browser check for F2-07 (machine details, rename, revoke). Paste into the console of a signed-in tab
// on a machine's page (/machines/<id>, Personal workspace) of a THROWAWAY machine you own, with
// `window.__machineId` set to its exact id and `window.__alias` to its current name. It compares
// the page with `GET /v1/machines/{id}` (name, owner "You", address on the configured local domain,
// machine ID), renames the machine from the page and restores the name, then removes it from the
// Remove dialog (the name must be typed) and checks the server no longer has it and the list does
// not show it. Only run it on a machine made for the check. Prints the steps (window.__checkSteps).
;(async () => {
  const id = window.__machineId
  const alias = window.__alias
  delete window.__machineId
  delete window.__alias
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(300)
    }
    return null
  }
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      try {
        const response = await fetch(`/api/v1/${path}`, { signal: AbortSignal.timeout(8000) })
        return { status: response.status, data: (await response.json().catch(() => ({}))).data }
      } catch (error) {
        if (attempt >= 4) throw error
        await sleep(600)
      }
    }
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim(), b))
  if (!id || !alias || !location.pathname.endsWith(id)) {
    pass("open the throwaway machine's page and set window.__machineId and __alias", false)
    return steps
  }

  const machine = (await get(`machines/${id}`)).data
  pass("the machine is yours and not shared", machine?.alias === alias && machine?.shared === false, machine?.alias)
  const config = (await get("client-configuration")).data
  const text = await until(() => (main()?.innerText.includes("Machine ID") ? main().innerText : null))
  pass(
    "details show the name, owner You and the machine ID",
    Boolean(text) && text.includes(alias) && /Owner\s+You/.test(text) && text.includes(id),
  )
  pass(
    "the address uses the configured local domain",
    Boolean(config?.local_access_base_domain) && text?.includes(`${alias}.${config.local_access_base_domain}`),
  )

  // Rename from the page, then put the name back.
  button(document, (t, b) => b.getAttribute("aria-label") === "Edit name and description")?.click()
  const nameInput = await until(() => [...main().querySelectorAll("input")].find((i) => i.value === alias))
  pass("Edit opens the name field", Boolean(nameInput))
  if (nameInput) {
    type(nameInput, `${alias}-x`)
    const save = await until(() => button(document, (t, b) => b.getAttribute("aria-label") === "Save"))
    save?.click()
    const renamed = await until(async () => (await get(`machines/${id}`)).data?.alias === `${alias}-x`)
    pass("rename is saved on the server", Boolean(renamed))
    // The title shows the new name before it can be edited again.
    await until(() => main().querySelector("h1")?.innerText === `${alias}-x`)
    button(document, (t, b) => b.getAttribute("aria-label") === "Edit name and description")?.click()
    const again = await until(() => [...main().querySelectorAll("input")].find((i) => i.value === `${alias}-x`))
    if (again) {
      type(again, alias)
      ;(await until(() => button(document, (t, b) => b.getAttribute("aria-label") === "Save")))?.click()
    }
    pass("the name is restored", Boolean(await until(async () => (await get(`machines/${id}`)).data?.alias === alias)))
  }
  // Remove only when the name really is back: the dialog asks for the name the machine has now.
  const current = (await get(`machines/${id}`)).data?.alias
  if (current !== alias) {
    pass("stopped before Remove because the name is not restored", false, current)
    return steps
  }
  await until(() => main().querySelector("h1")?.innerText === alias)

  // Remove: the dialog asks for the name and the machine is gone.
  button(main(), (t) => t === "Remove")?.click()
  const dialog = await until(() => document.querySelector("[role=dialog], [role=alertdialog]"))
  pass("Remove asks to type the machine name", Boolean(dialog) && dialog.innerText.includes(alias))
  const confirm = dialog && button(dialog, (t) => t === "Remove machine")
  pass("Remove machine is disabled until the name is typed", Boolean(confirm?.disabled))
  const field = dialog?.querySelector("input")
  if (field && confirm) {
    type(field, alias)
    await sleep(300)
    const typed = await until(() => !confirm.disabled)
    pass("Remove machine is enabled once the name is typed", Boolean(typed))
    if (!typed) return steps
    window.__removedAt = new Date().toISOString() // when the revoke was sent, for a device that watches for it
    confirm.click()
    const gone = await until(async () => (await get(`machines/${id}`)).status === 404, 20000)
    pass("the server no longer has the machine", Boolean(gone))
    const list = (await get("machines?workspace=personal")).data
    pass("the machine list no longer shows it", !(list?.items ?? []).some((m) => m.id === id))
  }
  return steps
})()
