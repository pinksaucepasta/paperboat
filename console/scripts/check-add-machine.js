// Browser check for F1-12 (Add machine dialog: the right command, live update when the machine
// connects). Paste into the console of a signed-in tab on /machines (Personal workspace). Opens the
// Add machine dialog, checks the pairing command against what the server publishes
// (GET /v1/machines/overview bootstrap_command) and against the enrollment it created
// (GET /v1/machine-enrollments/{id}, waiting), switches to "Approve in browser instead" (the same
// command without a token) and back, and closing the dialog cancels the unused enrollment.
//
// With `window.__connect = <loopback sink URL>` it keeps the dialog open after the first checks,
// sends the pairing command (one-shot token inside) to that sink (a loopback listener that stores it for the machine to run; `pb pair
// --enrollment-token-file`; the token is never printed or kept in the steps), and waits up to four
// minutes for the dialog to turn into "Machine connected" with an Open machine link, then checks
// the machine exists on the server. `window.__machineId` holds its id for cleanup. Prints the steps.
;(async () => {
  const sink = window.__connect
  delete window.__connect
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
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
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      try {
        return (await (await fetch(`/api/v1/${path}`, { signal: AbortSignal.timeout(8000) })).json()).data
      } catch (error) {
        if (attempt >= 4) throw error
        await sleep(600)
      }
    }
  }
  const dialog = () => document.querySelector("[role=dialog]")
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim()))
  const command = () => dialog()?.querySelector("code")?.textContent ?? ""

  const overview = await get("machines/overview")
  const installUrl = overview.bootstrap_command?.match(/https:\/\/[^\s'"]+\/install\b/)?.[0]
  pass("the server publishes an install address", Boolean(installUrl))

  // The dialog creates its enrollment with POST machine-enrollments; remember its id (not the token).
  const realFetch = window.fetch
  let enrollmentId = ""
  window.fetch = async (input, init) => {
    const response = await realFetch(input, init)
    const url = typeof input === "string" ? input : input.url
    if (init?.method === "POST" && /\/machine-enrollments$/.test(url)) {
      response
        .clone()
        .json()
        .then((body) => (enrollmentId = (body.data ?? body).id))
        .catch(() => {})
    }
    return response
  }
  try {
    return await run()
  } finally {
    window.fetch = realFetch
  }

  async function run() {
    const add = await until(() => button(document, (t) => t === "Add machine"))
    pass("Add machine is on the page", Boolean(add))
    if (!add || !installUrl) return steps
    add.click()
    const withToken = await until(() => /\?p=/.test(command()) && command())
    pass("the dialog shows a pairing command", Boolean(withToken))
    if (!withToken) return steps
    // Launch is macOS and Linux only (PLAN.md 1.1): the POSIX one-liner, no Windows command.
    const shape = new RegExp(
      `^curl -fsSL '${installUrl.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\?p=[A-Za-z0-9_.-]+' \\| bash$`,
    )
    pass("the command is the published install address with the token, piped to bash", shape.test(withToken))
    pass("no Windows command is offered yet", !/powershell/i.test(dialog().innerText))
    const token = withToken.match(/\?p=([^']+)'/)?.[1] ?? ""

    // The enrollment the dialog created is waiting for a machine.
    await until(() => enrollmentId)
    const waiting = enrollmentId && (await get(`machine-enrollments/${enrollmentId}`))
    pass(
      "the server has the enrollment waiting for a machine",
      waiting?.state === "awaiting_bootstrap",
      waiting?.state ?? "",
    )
    const label = await until(() => /Waiting for the machine/.test(dialog()?.innerText ?? "") || null)
    pass("the dialog says it is waiting for the machine", Boolean(label))

    // "Approve in browser instead": the plain command, no token, nothing to cancel on the server.
    button(dialog(), (t) => t === "Approve in browser instead")?.click()
    const plain = await until(() => command() && !/\?p=/.test(command()) && command())
    pass("the browser-approval command has no token", Boolean(plain) && plain.includes(installUrl))
    pass("it says what happens next", /opens your browser to sign in/.test(dialog().innerText))
    button(dialog(), (t) => t === "Use a pairing command")?.click()
    pass("switching back shows the pairing command", Boolean(await until(() => /\?p=/.test(command()))))

    if (sink) {
      await fetch(sink, { method: "POST", mode: "no-cors", body: withToken })
      pass("the command went to the loopback sink", true)
      const connected = await until(() => /Machine connected/.test(dialog()?.innerText ?? "") || null, 240000)
      pass("the dialog turns into Machine connected when the machine pairs", Boolean(connected))
      const open = dialog() && [...dialog().querySelectorAll("a")].find((a) => a.textContent.trim() === "Open machine")
      const id = open?.getAttribute("href")?.split("/").pop()
      pass("Open machine links to the new machine", Boolean(id))
      if (id) {
        window.__machineId = id
        const machine = await get(`machines/${id}`)
        pass("the machine exists on the server", machine?.id === id, machine?.alias ?? "")
        pass("it is in the Personal workspace", !machine?.owner_team_id)
      }
      button(dialog() ?? document, (t) => t === "Close")?.click()
      return steps
    }

    // Closing the dialog revokes the unused token.
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
    pass("closing the dialog closes it", Boolean(await until(() => !dialog())))
    const cancelled = await until(async () => (await get(`machine-enrollments/${enrollmentId}`)).state === "cancelled")
    pass("the unused enrollment is cancelled on the server", Boolean(cancelled))
    return steps
  }
})()
