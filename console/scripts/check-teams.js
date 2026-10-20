// Browser check for F12-07. Paste into the console of a signed-in tab on /team (Personal
// workspace; select Personal first with `document.cookie = "pb-workspace=personal; Path=/"` and a
// reload, since a selected team opens its page instead of the list). Opens the team named by `window.__team` (default paperboat-smoke-paid), compares the
// page with `GET /v1/teams/{id}` (members, roles, seats), exercises the invite dialog without
// sending anything (invalid address, seat shortfall with the Add seats link), renames the team and
// restores the name, and opens the member menu and the delete dialog without confirming either.
// It changes nothing that is left behind. Prints the steps.
;(async () => {
  const teamId = window.__team ?? "paperboat-smoke-paid"
  delete window.__team
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
    for (let waited = 0; waited < ms; waited += 200) {
      const found = await test()
      if (found) return found
      await sleep(200)
    }
    return null
  }
  const type = (input, text) => {
    const prototype = input instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype
    Object.getOwnPropertyDescriptor(prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const button = (root, test) => [...root.querySelectorAll("button")].find((b) => test(b.textContent.trim(), b))
  const dialog = () => document.querySelector("[role=dialog], [role=alertdialog]")
  const close = async () => {
    const d = dialog()
    const x = d && button(d, (text) => text === "Close" || text === "Cancel")
    x?.click()
    await sleep(500)
  }
  // The console is reached over a tunnel that drops a request now and then; retry reads.
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      try {
        return (await (await fetch(`/api/v1/${path}`)).json()).data
      } catch (error) {
        if (attempt >= 4) throw error
        await new Promise((resolve) => setTimeout(resolve, 600))
      }
    }
  }

  const list = await until(() => (main()?.innerText.includes("Your teams") ? main() : null))
  pass("teams list shown with Create team", Boolean(list) && Boolean(button(main(), (t) => t === "Create team")))
  const row = list && button(main(), (t) => t.includes(teamId))
  pass("retained team listed with the owner role", Boolean(row) && /Owner/.test(row?.textContent ?? ""))
  if (!row) return steps
  row.click()

  const header = await until(() => (main()?.innerText.includes("Danger zone") ? main().innerText : null))
  pass("team page opens", Boolean(header))
  if (!header) return steps
  const team = await get(`teams/${encodeURIComponent(teamId)}`)
  const active = team.members.filter((member) => member.active)
  pass("member count matches the server", header.includes(`${active.length} members`), `${active.length} active`)
  const names = active.map((member) => member.email)
  pass(
    "every active member listed with an email",
    names.every((email) => header.includes(email)),
  )
  const owners = active.filter((member) => member.role === "owner")
  pass("owner shown as You with the Owner role", owners.length === 1 && /You[\s\S]*Owner/.test(header))
  const billing = await get(`billing/workspaces/${encodeURIComponent(teamId)}`).catch(() => null)
  const seats = header.match(/(\d+) of (\d+) seats used/)
  pass("seats used and bought shown", Boolean(seats), seats?.[0] ?? "")
  if (billing && seats) {
    pass("seats match billing", Number(seats[1]) === billing.seats_used && Number(seats[2]) === billing.seats_bought)
  }

  // Invite dialog: nothing is sent.
  button(main(), (t) => t === "Invite members").click()
  const email = await until(() => document.getElementById("invite-emails"))
  pass("invite dialog opens", Boolean(email))
  if (email) {
    type(email, "not-an-email")
    await sleep(300)
    pass("invalid address is flagged", /Not an email: not-an-email/.test(dialog().innerText))
    pass("send is disabled for an invalid address", button(dialog(), (t) => /^Send invitation/.test(t)).disabled)
    type(email, "lanec-check@example.test")
    await sleep(300)
    const free = seats ? Number(seats[2]) - Number(seats[1]) : 0
    // The seat count arrives with the billing query, a moment after the dialog opens.
    const sentence = await until(() => {
      const text = dialog().innerText
      return (free === 0 ? /need 1 more seat/ : new RegExp(`${free} free seat`)).test(text) ? text : null
    }, 5000)
    pass(
      free === 0 ? "seat shortfall named, Add seats offered" : "free seats stated",
      Boolean(sentence) &&
        (free !== 0 || [...dialog().querySelectorAll("a, button")].some((e) => e.textContent.trim() === "Add seats")),
      sentence ? "" : dialog().innerText.replace(/\s+/g, " "),
    )
    await close()
  }

  // Rename and restore.
  const input = [...main().querySelectorAll("input")].find(
    (i) => i.value === (team.display_name || team.name || teamId),
  )
  if (input) {
    const original = input.value
    type(input, `${original}-check`)
    await sleep(200)
    button(main(), (t) => t === "Save").click()
    await sleep(2500)
    const after = await get(`teams/${encodeURIComponent(teamId)}`)
    pass("rename saved on the server", (after.display_name || after.name) === `${original}-check`)
    const again = [...main().querySelectorAll("input")].find((i) => i.value === `${original}-check`)
    type(again, original)
    await sleep(200)
    button(main(), (t) => t === "Save").click()
    await sleep(2500)
    const restored = await get(`teams/${encodeURIComponent(teamId)}`)
    pass("name restored", (restored.display_name || restored.name) === original)
  } else {
    pass("settings show the team name", false)
  }

  // Approve new machines: shown for owners/admins, flips on the server and is restored.
  const toggle = main().querySelector('[role="switch"][aria-label="Approve new machines"], #team-approval')
  pass("Approve new machines switch is shown", Boolean(toggle))
  if (toggle) {
    const current = (await get(`teams/${encodeURIComponent(teamId)}`)).approve_new_machines
    pass(
      "switch shows the server's value",
      toggle.getAttribute("aria-checked") === String(Boolean(current)),
      `server=${current}`,
    )
    toggle.click()
    const flipped = await until(
      async () => (await get(`teams/${encodeURIComponent(teamId)}`)).approve_new_machines === !current,
    )
    pass("turning it off/on is saved on the server", Boolean(flipped))
    ;(main().querySelector('[role="switch"][aria-label="Approve new machines"], #team-approval') ?? toggle).click()
    pass(
      "switch restored",
      Boolean(
        await until(async () => (await get(`teams/${encodeURIComponent(teamId)}`)).approve_new_machines === current),
      ),
    )
  }
  const waiting = main().innerText.includes("Machines waiting")
  const requests = (await get(`teams/${encodeURIComponent(teamId)}`)).machine_requests ?? []
  pass(
    "Machines waiting appears exactly when there are requests",
    waiting === requests.length > 0,
    `${requests.length} requests`,
  )

  // Member menu opens (no action taken).
  const actions = [...main().querySelectorAll("button")].find((b) =>
    b.getAttribute("aria-label")?.startsWith("Actions for"),
  )
  if (actions) {
    actions.click()
    const menu = await until(() => document.querySelector("[role=menu]"))
    pass(
      "member menu offers transfer and remove",
      Boolean(menu) && /Transfer ownership/.test(menu.innerText) && /Remove from team/.test(menu.innerText),
      menu?.innerText.replace(/\s+/g, " ") ?? "",
    )
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
    await sleep(500)
  } else {
    pass("member menu present", false)
  }

  // Delete dialog needs the team name typed; cancelled.
  const del = [...main().querySelectorAll("button")].filter((b) => b.textContent.trim() === "Delete team").at(-1)
  del.click()
  const confirmation = await until(() => dialog())
  pass("delete asks for confirmation", Boolean(confirmation))
  if (confirmation) {
    const confirmButton = button(confirmation, (t) => /^Delete team$/.test(t))
    pass("delete is disabled until the name is typed", !confirmButton || confirmButton.disabled)
    await close()
  }
  const still = await get(`teams/${encodeURIComponent(teamId)}`)
  pass("team still exists", Boolean(still?.team_id ?? still?.id ?? still))
  return steps
})()
