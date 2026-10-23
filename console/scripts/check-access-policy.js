// Browser check for F18-09 (workspace Access page and Settings → API tokens). Paste into the
// console of a signed-in tab, in the workspace under test (personal or a team you own or admin).
//
// On /access: the four tabs, the Simple view's tags against the file (GET .../policy), the
// editor holding the saved file, a broken draft getting inline errors (path and code) from
// .../policy/validate and a fixed one getting none, Test answering what POST .../policy/explain
// answers, and History listing what .../policy/versions lists. With `window.__save = true` it also
// saves a comment-only change (no access change, so no confirm) and restores the previous version
// from History, leaving the file as it was with two new versions.
//
// On /settings/api-tokens: creates a scoped token named check-<random>, checks the secret is shown
// once and the token is listed with its scope, revokes it and checks it reads Revoked. The secret
// is never printed or kept in the steps. Prints the steps.
;(async () => {
  const save = window.__save
  delete window.__save
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
  const post = (path, body) =>
    call(path, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) })
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim()))
  const tab = (name) => button(document, (t) => t.replace(/•$/, "").trim() === name)
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

  if (location.pathname.startsWith("/settings/api-tokens")) return tokens()

  const policy = await call(`${base}/policy`)
  pass("the server answers GET policy", policy.status === 200, `status ${policy.status}`)
  if (policy.status !== 200) {
    const unavailable = await until(() => /isn't available on this server yet/.test(document.body.innerText))
    pass("the page says the policy isn't available yet", Boolean(unavailable))
    return steps
  }
  for (const name of ["Simple", "Policy file", "Test", "History"])
    pass(`the ${name} tab is on the page`, Boolean(await until(() => tab(name))))

  // Simple: every tag in the file shows on its machines.
  tab("Simple").click()
  const tagged = Object.entries(
    JSON.parse(policy.data.policy.replace(/\/\/.*$/gm, "").replace(/,(\s*[}\]])/g, "$1")).tags ?? {},
  )
  const tagsShown = await until(() =>
    tagged.every(([tag]) => document.body.innerText.includes(tag.replace(/^tag:/, ""))),
  )
  pass("Simple shows the file's tags", tagged.length === 0 || Boolean(tagsShown), `${tagged.length} tags`)

  // Policy file: the saved text, and inline errors for a broken draft.
  tab("Policy file").click()
  const area = await until(() => document.querySelector("textarea[aria-label='Policy file']"))
  pass("the editor holds the saved file", area?.value === policy.data.policy)
  const editable = policy.data.can_edit === "all"
  pass("the editor is read-only unless you can edit", area?.readOnly === !editable, policy.data.can_edit)
  if (editable) {
    type(area, policy.data.policy.replace(/\{/, '{"no_such_field": 1,'))
    const invalid = await until(() => document.querySelector("[data-policy-validation=invalid]"))
    pass("a broken draft shows errors inline", Boolean(invalid))
    pass("the error names its path and code", /unknown_field/.test(invalid?.innerText ?? ""), invalid?.innerText ?? "")
    button(document, (t) => t === "Discard").click()
    pass(
      "discarding brings back a valid file",
      Boolean(await until(() => document.querySelector("[data-policy-validation=valid]"))),
    )
  }

  // Test: the page's answer is the server's.
  tab("Test").click()
  // The Test form is the one holding the "who" field (other forms can come first on the page).
  const form = await until(() => document.querySelector("#test-who")?.closest("form"))
  const who = document.querySelector("#test-who")?.value
  const target = document.querySelector("#test-target")?.value
  const action = document.querySelector("#test-action")?.value
  if (form && who && target) {
    button(form, (t) => t === "Test").click()
    const shown = await until(() => document.querySelector("[data-decision]"))
    const expected = await post(`${base}/policy/explain`, { src: `user:${who}`, dst: target, action })
    const want = expected.data?.decision?.allowed ? "allowed" : "denied"
    pass("Test answers what explain answers", shown?.dataset.decision === want, `${who} ${action} ${target}: ${want}`)
  } else pass("Test has someone and something to test", false)
  pass("the file's tests result is shown", Boolean(await until(() => document.querySelector("[data-policy-tests]"))))

  // History: the server's versions.
  tab("History").click()
  const versions = (await call(`${base}/policy/versions`)).data.versions
  const rows = await until(() => document.querySelectorAll("[data-version]").length === versions.length)
  pass("History lists every version", Boolean(rows), `${versions.length} versions`)

  if (save && editable) {
    const before = policy.data.version
    tab("Policy file").click()
    const editor = await until(() => document.querySelector("textarea[aria-label='Policy file']"))
    type(editor, `// check-access-policy ${Date.now()}\n${policy.data.policy}`)
    button(document, (t) => t === "Review changes").click()
    const review = await until(() => /Nobody gains or loses access/.test(dialog()?.innerText ?? "") && dialog())
    pass("a comment-only change changes no access", Boolean(review))
    button(dialog(), (t) => t === "Save")?.click()
    const saved = await until(async () => (await call(`${base}/policy`)).data.version === before + 1, 15000)
    pass("Save stores a new version", Boolean(saved))
    tab("History").click()
    const row = await until(() => document.querySelector(`[data-version="${before}"]`))
    button(row, (t) => t === "Restore")?.click()
    await until(() => dialog())
    button(dialog(), (t) => t.startsWith("Restore"))?.click()
    const restored = await until(async () => {
      const now = (await call(`${base}/policy`)).data
      return now.version === before + 2 && now.policy === policy.data.policy
    }, 15000)
    pass("Restore brings back the earlier file as a new version", Boolean(restored))
  }
  return steps

  async function tokens() {
    const list = await call(`${base}/api-tokens`)
    pass("the server answers GET api-tokens", list.status === 200, `status ${list.status}`)
    if (list.status !== 200) return steps
    const name = `check-${Math.random().toString(36).slice(2, 8)}`
    button(document, (t) => t === "Create token").click()
    const input = await until(() => document.querySelector("#token-name"))
    type(input, name)
    const policy = (await call(`${base}/policy`)).data
    if (policy?.names.machines.length) {
      dialog().querySelector("[role=checkbox]").click()
      const scope = await until(() => dialog().querySelectorAll("[role=checkbox]").length > 2)
      const boxes = [...dialog().querySelectorAll("label")]
      boxes
        .find((l) => l.textContent.trim() === policy.names.machines[0])
        ?.querySelector("[role=checkbox]")
        ?.click()
      boxes
        .find((l) => l.textContent.trim() === "Run commands")
        ?.querySelector("[role=checkbox]")
        ?.click()
      pass("the scope picker lists machines and actions", Boolean(scope))
    }
    button(dialog(), (t) => t === "Create token").click()
    const secret = await until(() => dialog()?.querySelector("[data-token-secret]")?.textContent)
    pass("the secret is shown once", /^pbat_/.test(secret ?? ""))
    pass("it says it is not shown again", /not shown again/.test(dialog()?.innerText ?? ""))
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
    const row = await until(() => document.querySelector(`[data-token="${name}"]`))
    pass("the new token is listed", Boolean(row))
    const created = (await call(`${base}/api-tokens`)).data.api_tokens.find((t) => t.name === name)
    pass(
      "the server has it, scoped",
      Boolean(created) && (!policy?.names.machines.length || created.scope?.length === 1),
    )
    button(row, (t) => t === "Revoke")?.click()
    await until(() => dialog())
    button(dialog(), (t) => t === "Revoke token")?.click()
    const revoked = await until(() => /Revoked/.test(document.querySelector(`[data-token="${name}"]`)?.innerText ?? ""))
    pass("revoking marks it Revoked", Boolean(revoked))
    const after = (await call(`${base}/api-tokens`)).data.api_tokens.find((t) => t.name === name)
    pass("the server has it revoked", Boolean(after?.revoked_at))
    return steps
  }
})()
