// Browser check for F7-06. Paste into the console of a signed-in tab on /previews (Personal
// workspace) with a service listening on the machine (set window.__port, default 18088; the
// machine is the dialog's default). Creates a preview from the New preview dialog, checks the
// list (address, forwards-to, end time, Live), stops it from the row menu and checks the server
// ends it, and leaves nothing running. The address stays in `window.__previewAddress` so a caller can
// confirm from outside the page (curl) that it served before the stop and no longer serves after. Prints the steps.
;(async () => {
  const port = window.__port ?? "18088"
  delete window.__port
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
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
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const button = (label, root = main()) =>
    [...root.querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const get = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data
  const previews = async () => (await get("previews?workspace=personal")).items
  const liveBefore = new Set((await previews()).filter((p) => p.state === "ready").map((p) => p.id))

  button("New preview")?.click()
  const create = await until(() => dialog()?.querySelector("#preview-port"))
  pass("New preview dialog opens", Boolean(create))
  if (!create) return steps
  type(create, port)
  await sleep(300)
  button("Create preview", dialog())?.click()
  const live = await until(() => /Preview is live\s+(\S+)/.exec(dialog()?.innerText ?? "")?.[1], 90000)
  pass("dialog reports the preview live with its address", Boolean(live), live ?? "")
  if (!live) return steps
  window.__previewAddress = live
  const created = await until(async () => (await previews()).find((p) => !liveBefore.has(p.id) && p.state === "ready"))
  pass("server has one new ready preview", Boolean(created))
  button("Done", dialog())?.click()
  await sleep(800)
  const row = await until(() => [...main().querySelectorAll("tr")].find((tr) => tr.innerText.includes(live)))
  pass(
    "list shows the address, forwards-to and an end time",
    Boolean(row) && row.innerText.includes(`localhost:${port}`) && /in \d+ (minute|hour)/.test(row.innerText),
    row?.innerText.replace(/\s+/g, " ").trim() ?? "",
  )
  const reach = () =>
    fetch(`https://${live}/`, { mode: "no-cors", cache: "no-store" }).then(
      () => true,
      () => false,
    )
  pass("the address answers", Boolean(await until(reach, 30000)))
  pass("status becomes Live", Boolean(await until(() => /Live/.test(row?.innerText ?? ""), 30000)))

  const actions = [...main().querySelectorAll("button")].find(
    (b) => b.getAttribute("aria-label") === `Actions for ${live}`,
  )
  actions?.click()
  const stop = await until(() =>
    [...document.querySelectorAll("[role=menuitem]")].find((i) => /Stop preview/.test(i.textContent)),
  )
  stop?.click()
  const confirm = await until(() => dialog())
  pass("stop asks to confirm", Boolean(confirm) && /stops working right away/.test(confirm.innerText))
  ;[...(confirm?.querySelectorAll("button") ?? [])].find((b) => /^Stop preview/.test(b.textContent.trim()))?.click()
  pass(
    "server ends the preview",
    Boolean(await until(async () => (await previews()).find((p) => p.id === created?.id)?.state === "stopped")),
  )
  pass(
    "preview leaves the Live list",
    Boolean(
      await until(
        () =>
          ![...main().querySelectorAll("tr")].some(
            (tr) => tr.innerText.includes(live) && /Live|Starting/.test(tr.innerText),
          ),
      ),
    ),
  )
  return steps
})()
