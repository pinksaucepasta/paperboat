// Browser check for F19 part 1 (roles and the audit log) against the live server.
// Paste into a signed-in tab of the console whose account owns the team in window.__team (default
// paperboat-smoke-paid). It switches the workspace cookie to the team (and back to what it was),
// reads Team and Activity, opens menus without choosing anything, and downloads one audit
// export (which the server records as team.activity_exported; the audit log is append-only, so the check
// counts those events relative to its own start and expects exactly one more). Changes nothing else. Prints steps.
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
  const get = async (path) => {
    const response = await fetch(`/api/v1/${path}`)
    const body = await response.json().catch(() => null)
    return { status: response.status, data: body?.data, error: body?.error }
  }
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const text = () => (main()?.innerText ?? "").replace(/\s+/g, " ")
  const click = (el) => el.dispatchEvent(new MouseEvent("click", { bubbles: true }))
  const open = async (path, ready) => {
    window.next.router.push(path)
    return until(() => location.pathname === path && ready())
  }
  const before = document.cookie.match(/(?:^|;\s*)pb-workspace=([^;]+)/)?.[1] ?? "personal"
  const useWorkspace = async (id) => {
    document.cookie = `pb-workspace=${id}; Path=/`
    window.next.router.refresh()
    await sleep(1500)
  }

  try {
    // Roles: the server's matrix has the five roles of teams-business.md.
    const roles = await get("team-roles")
    pass(
      "GET /v1/team-roles lists owner, admin, billing, auditor, member",
      ["owner", "admin", "billing", "auditor", "member"].every((role) => roles.data?.roles?.includes(role)),
      JSON.stringify(roles.data?.roles),
    )

    await useWorkspace(teamId)

    // Members: the owner's role menu offers the four roles with their descriptions.
    const members = (await get(`teams/${teamId}/members?limit=200`)).data?.items
    const target = members?.find((m) => m.active && m.role !== "owner")
    const page = await open("/team", () => /Your role/.test(text()))
    pass("team page opens", Boolean(page))
    if (target) {
      const trigger = await until(() =>
        [...main().querySelectorAll("button")].find(
          (b) => /^(Admin|Member|Billing admin|Auditor)$/.test(b.textContent.trim()) && b.closest("tr"),
        ),
      )
      if (trigger) {
        trigger.focus()
        trigger.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, button: 0 }))
        click(trigger)
        const items = await until(() => {
          const found = [...document.querySelectorAll("[role=menuitemradio]")].map((i) => i.textContent)
          return found.length ? found : null
        }, 5000)
        pass(
          "the role menu offers Admin, Billing admin, Auditor and Member with what each does",
          Boolean(items) &&
            ["Admin", "Billing admin", "Auditor", "Member"].every((label) => items.some((i) => i.startsWith(label))) &&
            items.some((i) => /invoices/.test(i)),
          JSON.stringify(items),
        )
        document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
        await sleep(400)
      } else pass("the role menu opens", false, "no role button")
    }

    // Activity: filters reach the server, and the export downloads the same events.
    const seen = []
    const realFetch = window.fetch
    window.fetch = (input, init) => {
      const url = String(input.url ?? input)
      if (url.includes("/api/v1/activity?")) seen.push(url)
      return realFetch(input, init)
    }
    try {
      // Choose right after arriving, on the static shell the server page is about to replace (a client navigation shows the
      // shell first): the filter must live through that swap. Export (owner only) shows once the workspace role is known.
      await open(
        "/activity",
        () => /Any time/.test(text()) && [...main().querySelectorAll("button")].some((b) => b.textContent.trim() === "Export"),
      )
      const choose = async (current, wanted) => {
        const trigger = [...main().querySelectorAll("button")].find((b) => b.textContent.trim() === current)
        if (!trigger) return false
        trigger.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, button: 0 }))
        click(trigger)
        const item = await until(
          () => [...document.querySelectorAll("[role=menuitemradio],[role=menuitem],[role=option]")].find((i) => i.textContent.trim() === wanted),
          5000,
        )
        if (!item) return false
        click(item)
        // The trigger shows the choice once the page took it.
        return Boolean(await until(() => [...main().querySelectorAll("button")].some((b) => b.textContent.trim() === wanted), 5000))
      }
      pass("Last 7 days can be chosen", await choose("Any time", "Last 7 days"))
      // The server page has replaced the shell once nothing is hidden behind the shell delay and no skeleton is left.
      await until(() => !document.querySelector(".shell-delay") && !main().querySelector('[data-slot="skeleton"]'), 30000)
      pass(
        "the filter chosen on arrival survives the server page replacing the shell",
        [...main().querySelectorAll("button")].some((b) => b.textContent.trim() === "Last 7 days") && /[?&]period=week/.test(location.search),
        location.search,
      )
      pass("the list asks the server for the period", seen.some((url) => /[?&]since=/.test(url)), seen.at(-1) ?? "")
      pass("Only denied can be chosen", await choose("All results", "Only denied"))
      pass("the list asks for denied events only", seen.some((url) => /result=denied/.test(url)), seen.at(-1) ?? "")
      const exportButton = [...main().querySelectorAll("button")].find((b) => b.textContent.trim() === "Export")
      pass("Export is offered to the team owner", Boolean(exportButton))
      if (exportButton) {
        // Ids, not a count: the list is capped at one page, so a count stops growing on a busy team.
        const exported = async () =>
          (await get(`activity?workspace=${teamId}&action=team.activity_exported&limit=200`)).data?.items?.map((e) => e.id) ?? []
        const exportedBefore = await exported()
        // The menu opens on a click; open it again if it did not (a click that lands while the page re-renders is lost).
        let csv = null
        for (let attempt = 0; attempt < 3 && !csv; attempt++) {
          if (!document.querySelector("[role=menu]")) {
            exportButton.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, button: 0 }))
            click(exportButton)
          }
          csv = await until(() => [...document.querySelectorAll("a[download]")].find((a) => /format=csv/.test(a.href)), 4000)
        }
        pass("Export offers CSV with the same filters", Boolean(csv) && /result=denied/.test(csv?.href ?? "") && /since=/.test(csv?.href ?? ""))
        if (csv) {
          const response = await realFetch(csv.href)
          const head = (await response.text()).split("\n")[0]
          pass("the CSV downloads with the documented columns", response.ok && head.startsWith("id,occurred_at,workspace,action,result"), `${response.status} ${head.slice(0, 60)}`)
          const added = async () => (await exported()).filter((id) => !exportedBefore.includes(id))
          const recorded = await until(async () => (await added()).length >= 1, 8000)
          pass("the export is recorded once as team.activity_exported", Boolean(recorded) && (await added()).length === 1, `${(await added()).length} new`)
        }
        document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }))
      }
    } finally {
      window.fetch = realFetch
    }

    // Personal workspaces have no export (the server refuses it).
    const personal = await realFetchless(`/api/v1/activity/export?workspace=personal&format=json`)
    pass("personal export is refused with activity_export_team_only", personal === "activity_export_team_only", personal)
  } finally {
    await useWorkspace(before)
  }
  return steps

  async function realFetchless(url) {
    const response = await fetch(url)
    const body = await response.json().catch(() => null)
    return body?.error?.code ?? String(response.status)
  }
})()
