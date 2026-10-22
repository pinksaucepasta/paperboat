// Browser check for F12-06 (console side). Paste into a signed-in tab on /team (Personal selected).
// Opens the team named by window.__team (default paperboat-smoke-paid) with the server's answers
// replaced locally: the team reads suspended, and a change answers 409 team_suspended. Checks the
// suspended notice, and that a rename the server refuses says the team is suspended and who can
// restore it (the console's sentence, not the server's raw message). Nothing reaches the server.
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const teamId = window.__team ?? "paperboat-smoke-paid"
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(200)
    }
    return null
  }
  const real = window.fetch
  const json = (status, body) =>
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })
  window.fetch = async (input, init) => {
    const url = String(input.url ?? input)
    const method = (init?.method ?? input.method ?? "GET").toUpperCase()
    if (new RegExp(`/api/v1/teams/${teamId}/actions$`).test(url) && method === "POST")
      return json(409, { error: { code: "team_suspended", message: "raw server message" } })
    const response = await real(input, init)
    if (new RegExp(`/api/v1/teams/${teamId}$`).test(url) && method === "GET") {
      const body = await response.clone().json()
      return json(200, { ...body, data: { ...body.data, suspended: true } })
    }
    return response
  }
  try {
    const type = (input, text) => {
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
      input.dispatchEvent(new Event("input", { bubbles: true }))
    }
    const main = () => document.querySelector("main main") ?? document.querySelector("main")
    const list = await until(() => (main()?.innerText.includes("Your teams") ? main() : null))
    const row = list && [...main().querySelectorAll("button")].find((b) => b.textContent.includes(teamId))
    pass("the team is listed", Boolean(row))
    if (!row) return steps
    row.click()
    const notice = await until(() => /This team is suspended/.test(main()?.innerText ?? "") && main())
    pass("the suspended notice shows", Boolean(notice))
    const input = [...main().querySelectorAll("input")].find((i) => i.value === teamId && !i.disabled)
    pass("the name field is editable", Boolean(input))
    type(input, `${input.value}-x`)
    await sleep(200)
    ;[...main().querySelectorAll("button")].find((b) => b.textContent.trim() === "Save").click()
    const shown = await until(() => /Ask the team owner to restore billing or resume the team/.test(document.body.innerText))
    pass("the refused change says the team is suspended and who restores it",
      Boolean(shown) && /This team is suspended, so changes are paused/.test(document.body.innerText))
    pass("the server's raw message is not shown", !/raw server message/.test(document.body.innerText))
  } finally {
    window.fetch = real
  }
  return steps
})()
