// Browser check for F26 (Settings → Recipes). Paste into the console of a signed-in tab on
// /settings/recipes, in the workspace under test (personal or a team you own or admin).
//
// Sends a file with a mistake (the line:column error and its suggestion show), creates the recipe from
// the corrected file, checks the token is shown once (67 characters, pbr_ prefix) with the install lines,
// opens the recipe, rotates it (type the code, a different token), and revokes it (type the code; it
// leaves the list and the server's). The token is never printed or kept in the steps. Prints the steps.
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(300)
    }
    return null
  }
  const call = async (path) => {
    const response = await fetch(`/api/v1/${path}`)
    return { status: response.status, data: (await response.json()).data }
  }
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim()))
  const link = (test) => [...document.querySelectorAll("main a")].find((a) => test(a.textContent.trim()))
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const type = (element, value) => {
    const proto = element instanceof HTMLTextAreaElement ? HTMLTextAreaElement : HTMLInputElement
    Object.getOwnPropertyDescriptor(proto.prototype, "value").set.call(element, value)
    element.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const workspaces = (await call("workspaces")).data.items
  const chosen = document.cookie.match(/(?:^|;\s*)pb-workspace=([a-z0-9-]{1,63})/)?.[1] ?? "personal"
  const ws = (workspaces.find((w) => w.id === chosen) ?? workspaces.find((w) => w.kind === "personal")).id
  const list = `workspaces/${encodeURIComponent(ws)}/recipes`
  const name = `check-${Math.random().toString(36).slice(2, 8)}`

  const listed = await call(list)
  pass("the server answers GET recipes", listed.status === 200, `status ${listed.status}`)
  if (listed.status !== 200) return steps

  ;(await until(() => link((t) => t === "New recipe"))).click()
  type(await until(() => document.querySelector("#recipe-name")), name)
  const file = await until(() => document.querySelector("#recipe-file"))
  type(file, 'tgs = ["check"]\nmax_machines = 2\n')
  ;(await until(() => button(document, (t) => t === "Create recipe"))).click()
  const errors = await until(() => document.querySelector("[data-recipe-errors]")?.innerText)
  pass("a mistake shows its line, column and suggestion", /^1:1 .*tags/s.test(errors ?? ""), errors ?? "")
  pass("nothing was created yet", !(await call(list)).data.recipes.some((r) => r.name === name))

  type(file, 'tags = ["check"]\nmax_machines = 2\n')
  ;(await until(() => button(document, (t) => t === "Create recipe"))).click()
  const token = await until(() => document.querySelector("[data-recipe-token]")?.textContent?.trim())
  pass("the token is shown once", /^pbr_[a-z2-7]{10}_[a-z2-7]{52}$/.test(token ?? ""))
  pass("it says it is not shown again", /won’t see it again/.test(document.querySelector("main").innerText))
  pass("the install lines are shown", /PB_RECIPE=/.test(document.querySelector("main").innerText))
  button(document, (t) => t === "I saved it").click()

  await until(() => document.querySelector("[data-recipe-file]"))
  pass("the recipe opens with its file", /max_machines = 2/.test(document.querySelector("[data-recipe-file]")?.textContent ?? ""))
  pass("the server has it at revision 1", (await call(`${list}/${name}`)).data.recipe.revision === 1)

  button(document.querySelector("main"), (t) => t === "Rotate token").click()
  const code = await until(() => dialog()?.querySelector("label span.font-mono")?.textContent)
  pass("rotating asks for the code", Boolean(code))
  type(dialog().querySelector("input"), code)
  await sleep(300)
  button(dialog(), (t) => t === "Rotate token").click()
  const next = await until(() => dialog()?.querySelector("[data-recipe-token]")?.textContent?.trim())
  pass("rotating shows a different token", /^pbr_/.test(next ?? "") && next !== token)
  button(dialog(), (t) => t === "I saved it").click()
  await until(() => !dialog())

  button(document, (t) => t === "Revoke").click()
  const revokeCode = await until(() => dialog()?.querySelector("label span.font-mono")?.textContent)
  pass("revoking says what goes and asks for the code", Boolean(revokeCode) && /token stops working/.test(dialog().innerText))
  type(dialog().querySelector("input"), revokeCode)
  await sleep(300)
  button(dialog(), (t) => t === "Revoke recipe").click()
  const gone = await until(() => location.pathname === "/settings/recipes" && !document.querySelector(`[data-recipe="${name}"]`))
  pass("a revoked recipe leaves the list", Boolean(gone))
  pass("the server no longer lists it", !(await call(list)).data.recipes.some((r) => r.name === name))
  return steps
})()
