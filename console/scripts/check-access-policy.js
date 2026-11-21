// Browser check for F18-09 (Settings → API tokens). Paste into the
// console of a signed-in tab, in the workspace under test (personal or a team you own or admin).
//
// On /settings/api-tokens: creates a scoped token named check-<random> on the Create token page, checks the secret
// is shown once and the token is listed with its scope, revokes it and checks it reads Revoked under Revoked and expired. The secret
// is never printed or kept in the steps. Prints the steps.
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
  // The console is reached over a tunnel that drops a request now and then.
  const call = async (path, init) => {
    for (let attempt = 0; ; attempt++) {
      try {
        const response = await fetch(`/api/v1/${path}`, { signal: AbortSignal.timeout(8000), ...init })
        return { status: response.status, data: (await response.json()).data }
      } catch (error) {
        if (attempt >= 4) throw error
        await sleep(600)
      }
    }
  }
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim()))
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const type = (element, value) => {
    const proto = element instanceof HTMLTextAreaElement ? HTMLTextAreaElement : HTMLInputElement
    Object.getOwnPropertyDescriptor(proto.prototype, "value").set.call(element, value)
    element.dispatchEvent(new Event("input", { bubbles: true }))
  }

  const workspaces = (await call("workspaces")).data.items
  const chosen = document.cookie.match(/(?:^|;\s*)pb-workspace=([a-z0-9-]{1,63})/)?.[1] ?? "personal"
  const ws = (workspaces.find((w) => w.id === chosen) ?? workspaces.find((w) => w.kind === "personal")).id
  const base = `workspaces/${encodeURIComponent(ws)}`

  return tokens()

  async function tokens() {
    const list = await call(`${base}/api-tokens`)
    pass("the server answers GET api-tokens", list.status === 200, `status ${list.status}`)
    if (list.status !== 200) return steps
    const name = `check-${Math.random().toString(36).slice(2, 8)}`
    // Every token of one state, across the server's pages.
    const all = async (state) => {
      const found = []
      for (let cursor = ""; ; ) {
        const page = (await call(`${base}/api-tokens?limit=50&state=${state}${cursor && `&cursor=${encodeURIComponent(cursor)}`}`)).data
        found.push(...page.api_tokens)
        if (!(cursor = page.next_cursor)) return found
      }
    }
    const choose = (element, value) => {
      Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value").set.call(element, value)
      element.dispatchEvent(new Event("change", { bubbles: true }))
    }
    // Creating a token is its own page: Custom, a name, one permission on all machines, the summary, the secret once.
    document.querySelector('main a[href$="/settings/api-tokens/new"]').click()
    ;(await until(() => button(document, (t) => t.startsWith("Custom")))).click()
    type(await until(() => document.querySelector("#token-name")), name)
    const allow = await until(() => document.querySelector('select[aria-label="Run commands access"]'))
    pass("the scope picker lists machines and actions", Boolean(allow) && Boolean(document.querySelector('[role=radiogroup][aria-label="Machines"]')))
    choose(allow, "allowed")
    ;(await until(() => button(document, (t) => t === "Continue to summary" && !document.querySelector("button[type=submit][disabled]")))).click()
    ;(await until(() => button(document, (t) => t === "Create token"))).click()
    const secret = await until(() => document.querySelector("[data-token-secret]")?.textContent)
    pass("the secret is shown once", /^pbat_/.test((secret ?? "").trim()))
    pass("it says it is not shown again", /won’t see it again/.test(document.querySelector("main").innerText))
    ;[...document.querySelectorAll("main a")].find((a) => a.textContent.trim() === "Done").click()
    const row = await until(() => document.querySelector(`[data-token="${name}"]`))
    pass("the new token is listed", Boolean(row))
    const created = (await all("active")).find((t) => t.name === name)
    pass("the server has it, scoped", Boolean(created) && created.scope?.length === 1)
    // The list may have re-rendered since the row was found: click the current row's button.
    button(document.querySelector(`[data-token="${name}"]`), (t) => t === "Revoke")?.click()
    const confirm = await until(() => dialog() && button(dialog(), (t) => t === "Revoke token"))
    pass("revoking asks for confirmation", Boolean(confirm))
    if (!confirm) return steps
    confirm.click()
    // A revoked token leaves Active for the folded "Revoked and expired" list.
    const gone = await until(() => !document.querySelector(`[data-token="${name}"]`))
    ;(await until(() => button(document, (t) => t.startsWith("Revoked and expired")))).click()
    const revoked = await until(() => {
      button(document, (t) => t === "Load more")?.click()
      return /Revoked/.test(document.querySelector(`[data-token="${name}"]`)?.innerText ?? "")
    }, 30000)
    pass("revoking marks it Revoked", Boolean(gone) && Boolean(revoked))
    const after = (await all("ended")).find((t) => t.name === name)
    pass("the server has it revoked", Boolean(after?.revoked_at))
    return steps
  }
})()
