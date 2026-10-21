// Browser check for F12-04 (default access shown while a machine enrolls into a team). Paste into the
// console of a signed-in tab on /machines of an account that has at least one team. Opens the Add
// machine dialog, picks a team in the Workspace list and checks the "Who can use it" line (nobody
// until an owner or admin grants access in Machine access, the team's name, no grant implied), then
// picks Personal and checks the line is gone. Enrollments the dialog creates are cancelled when it
// closes; the check confirms that on the server. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
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
  const note = () => dialog()?.querySelector("[data-new-machine-access]")
  const radio = (name) =>
    [...(dialog()?.querySelectorAll("[role=radio]") ?? [])].find((r) => r.textContent.includes(name))

  const workspaces = (await get("workspaces")).items
  const team = workspaces.find((w) => w.kind === "team")
  pass("the account has a team to enroll into", Boolean(team), team?.name ?? "")
  if (!team) return steps

  const realFetch = window.fetch
  const enrollments = []
  window.fetch = async (input, init) => {
    const response = await realFetch(input, init)
    const url = typeof input === "string" ? input : String(input.url ?? input)
    if (init?.method === "POST" && /\/machine-enrollments$/.test(url))
      response
        .clone()
        .json()
        .then((body) => enrollments.push((body.data ?? body).id))
        .catch(() => {})
    return response
  }
  try {
    const add = await until(() =>
      [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === "Add machine"),
    )
    pass("Add machine is on the page", Boolean(add))
    add.click()
    await until(() => dialog() && radio(team.name))
    pass("the dialog offers the team", Boolean(radio(team.name)))
    pass("no line about access while Personal is chosen", !note())

    radio(team.name).click()
    const shown = await until(() => note())
    const text = shown?.innerText.replace(/\s+/g, " ") ?? ""
    pass("choosing the team shows who can use the machine", Boolean(shown), text)
    pass(
      "it says nobody yet, until an owner or admin grants access",
      /Nobody yet/.test(text) && /owner or admin/.test(text),
    )
    pass("it names the team and Machine access", text.includes(team.name) && /Machine access/.test(text))
    pass("it does not promise access", !/everyone|all members can/i.test(text))

    radio("Personal").click()
    pass("choosing Personal removes the line", Boolean(await until(() => !note())))

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
    pass("closing the dialog closes it", Boolean(await until(() => !dialog())))
    const cancelled = await until(async () => {
      const states = await Promise.all(enrollments.map(async (id) => (await get(`machine-enrollments/${id}`)).state))
      return enrollments.length > 0 && states.every((state) => state === "cancelled") && states
    }, 20000)
    const states = await Promise.all(enrollments.map(async (id) => (await get(`machine-enrollments/${id}`)).state))
    pass("every unused enrollment is cancelled on the server", Boolean(cancelled), states.join(", "))
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
