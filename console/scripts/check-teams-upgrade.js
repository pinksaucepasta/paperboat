// Browser check for the Teams prompt in Personal (pricing decisions 2026-10-22). Paste into a
// signed-in tab with Personal selected, on the page of a machine you own (or set window.__machine).
// Checks: the machine page's Share opens the Teams prompt (what a team adds, sharing a terminal or
// preview stays free, your teams to switch to); its Create team goes to /team and opens Create
// team, with no reopen after closing; /team's Add member opens the same prompt. Sends nothing.
// With billingLive.teamTrial on, the prompt and Create team also name the trial (checked here
// when the personal overview has team_trial_days > 0).
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
  const button = (root, text) => [...root.querySelectorAll("button")].find((b) => b.textContent.trim() === text)
  const dialog = () => document.querySelector("[role=dialog]")
  const machine = window.__machine ?? location.pathname.match(/^\/machines\/([^/]+)/)?.[1]
  delete window.__machine
  if (!machine) return [{ step: "on a machine page", ok: false }]
  const billing = (await (await fetch("/api/v1/billing/workspaces/personal")).json()).data
  const trial = billing?.team_trial_days

  if (!location.pathname.startsWith(`/machines/${machine}`)) window.next.router.push(`/machines/${machine}`)
  const share = await until(() => button(document, "Share"))
  pass("a machine you own in Personal has Share", Boolean(share))
  if (!share) return steps
  share.click()
  const prompt = await until(() => dialog()?.innerText.includes("Share machines with a team") && dialog())
  const text = prompt?.innerText ?? ""
  pass("Share opens the Teams prompt", Boolean(prompt))
  pass("it says what a team adds", /lasting access to each other’s machines/.test(text))
  pass("it says one terminal session or preview link stays free", /Sharing one terminal session or one preview link stays free/.test(text))
  if (trial > 0) pass("it names the trial", text.includes(`Free for ${trial} days`) && Boolean(button(prompt, `Start ${trial}-day free trial`)))
  const workspaces = (await (await fetch("/api/v1/workspaces")).json()).data
  const teams = (workspaces.items ?? workspaces).filter((w) => w.kind === "team")
  pass("your teams are offered", teams.every((t) => Boolean(button(prompt, t.name ?? t.id))), `${teams.length} teams`)

  button(prompt, trial > 0 ? `Start ${trial}-day free trial` : "Create team").click()
  const create = await until(() => location.pathname === "/team" && /Team ID/.test(dialog()?.innerText ?? "") && dialog())
  pass("its action opens Create team on /team", Boolean(create))
  await until(() => location.search === "", 20000)
  pass("the create parameter is removed", location.search === "")
  if (create) {
    ;[...create.querySelectorAll("button")].find((b) => /Close/.test(b.textContent))?.click()
    await sleep(2500)
    pass("closing Create team keeps it closed", !dialog())
  }
  const add = await until(() => button(document, "Add member"))
  pass("/team in Personal has Add member", Boolean(add))
  add?.click()
  pass("Add member opens the Teams prompt", Boolean(await until(() => dialog()?.innerText.includes("Add people with a team"))))
  button(dialog(), "Not now")?.click()
  pass("Not now closes it", Boolean(await until(() => !dialog(), 5000)))
  return steps
})()
