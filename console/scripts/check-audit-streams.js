// Browser check for Activity → Streaming (F19 audit log streaming) against the live server. Paste
// into a signed-in tab of the console whose account owns window.__team (default
// paperboat-smoke-paid). It compares the Streaming view with GET /v1/teams/{id}/audit-streams,
// opens each destination's Set up / Edit dialog and closes it without saving. It never creates a
// destination, so no audit event leaves Paperboat. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const teamId = window.__team ?? "paperboat-smoke-paid"
  delete window.__team
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
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const text = () => (main()?.innerText ?? "").replace(/\s+/g, " ")
  const buttons = (root) => [...root.querySelectorAll("button")]
  const before = document.cookie.match(/(?:^|;\s*)pb-workspace=([^;]+)/)?.[1] ?? "personal"
  try {
    const response = await fetch(`/api/v1/teams/${teamId}/audit-streams`)
    const streams = (await response.json()).data
    pass("GET audit-streams answers", response.ok && Array.isArray(streams), `${response.status}`)

    document.cookie = `pb-workspace=${teamId}; Path=/`
    window.next.router.push("/activity")
    const toggle = await until(() => [...main().querySelectorAll("button,[role=radio]")].find((b) => b.textContent.trim() === "Streaming"))
    pass("Activity offers Events | Streaming to the team owner", Boolean(toggle))
    if (!toggle) return steps
    toggle.click()
    const shown = await until(() => /HTTPS/.test(text()) && /S3-compatible storage/.test(text()) && text())
    pass("both destinations are listed", Boolean(shown))

    for (const [kind, title] of [
      ["https", "HTTPS"],
      ["s3", "S3-compatible storage"],
    ]) {
      const stream = streams.find((s) => s.kind === kind)
      const section = [...main().querySelectorAll("section")].find((s) => s.querySelector("h2")?.textContent.trim().startsWith(title))
      const sectionText = section?.innerText.replace(/\s+/g, " ") ?? ""
      pass(
        `${title}: the state matches the server`,
        stream ? sectionText.includes(kind === "s3" ? stream.bucket : stream.url) : /Not set up/.test(sectionText),
        stream ? "configured" : "not set up",
      )
      const open = section && buttons(section).find((b) => b.textContent.trim() === (stream ? "Edit" : "Set up"))
      if (!open) {
        pass(`${title}: Set up/Edit is offered`, false)
        continue
      }
      open.click()
      const dialog = await until(() => document.querySelector("[role=dialog]"))
      const fields = [...(dialog?.querySelectorAll("input") ?? [])].map((i) => i.id)
      pass(
        `${title}: the dialog asks for the destination`,
        kind === "https"
          ? fields.includes("stream-url") && fields.includes("stream-header_name")
          : ["stream-url", "stream-region", "stream-bucket", "stream-access_key_id", "stream-secret_access_key"].every((f) => fields.includes(f)),
        fields.join(","),
      )
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
      await until(() => !document.querySelector("[role=dialog]"), 5000)
    }
  } finally {
    document.cookie = `pb-workspace=${before}; Path=/`
  }
  return steps
})()
