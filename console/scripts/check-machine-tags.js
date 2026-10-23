// Browser check for machine tags against the live server. Run in a signed-in tab (e.g. a headless
// Chromium as a throwaway account) whose Personal workspace has a machine; window.__machine picks
// one (default: the first). It adds the throwaway tag tag:console-check from the machine page's
// Edit tags, checks GET /v1/machines/{id} and the list's chip and tag filter, then removes it again
// and checks it is gone. Each save is one policy version. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const tag = "tag:console-check"
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const get = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data
  const dialog = () => document.querySelector("[role=dialog]")
  const button = (root, label) => [...(root?.querySelectorAll("button") ?? [])].find((b) => b.textContent.trim() === label)
  const machineTags = async (id) => (await get(`machines/${id}`))?.tags ?? []

  const tags = await get("workspaces/personal/tags")
  pass("GET tags answers with can_edit", Boolean(tags?.can_edit), tags?.can_edit ?? "none")
  const id = window.__machine ?? (await get("machines?limit=1&workspace=personal")).items[0]?.id
  pass("a machine to tag", Boolean(id), id ?? "none")
  if (!id) return steps
  if ((await machineTags(id)).includes(tag)) pass("the throwaway tag was not left over", false)

  const edit = async (change) => {
    window.next.router.push(`/machines/${id}`)
    const open = await until(() => button(document.querySelector("[data-machine-tags]"), "Edit tags"))
    open?.click()
    await until(dialog)
    await change()
    button(dialog(), "Save")?.click()
    await until(() => !dialog(), 15000)
    return !dialog() || dialog().innerText.replace(/\s+/g, " ").slice(0, 160)
  }

  const added = await edit(async () => {
    const input = dialog().querySelector('input[aria-label="New tag"]')
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, "console-check")
    input.dispatchEvent(new Event("input", { bubbles: true }))
    await sleep(200)
    button(dialog(), "Add")?.click()
    await until(() => dialog().innerText.includes("console-check"))
  })
  pass("Edit tags saves a new tag", added === true, added === true ? "" : String(added))
  pass("the server has the tag", (await until(async () => (await machineTags(id)).includes(tag))) === true)
  const chip = await until(() => document.querySelector(`[data-machine-tags] a[href*="console-check"]`))
  pass("the machine page shows the tag as a chip", Boolean(chip))

  chip?.click()
  // The list applies ?q= once the page is shown; wait for the filtered rows.
  const rows = () => document.querySelectorAll("#machines-table tbody tr").length
  const filtered = await until(() => location.pathname === "/" && rows() === 1, 10000)
  pass("the chip opens the list filtered by the tag", Boolean(filtered), `${location.search} ${rows()} rows`)

  const removed = await edit(async () => {
    const label = [...dialog().querySelectorAll("label")].find((l) => l.textContent.trim() === "console-check")
    label?.querySelector("button,[role=checkbox]")?.click()
    await sleep(300)
  })
  pass("Edit tags removes it", removed === true, removed === true ? "" : String(removed))
  pass("the server no longer has it", (await until(async () => !(await machineTags(id)).includes(tag))) === true)
  return steps
})()
