// Browser check for Team → Security (F19 SSO and directory sync) against the live server.
// Paste into a signed-in tab of the console whose account owns window.__team (default
// paperboat-smoke-paid). Opens /team?team=<id>&tab=security (where the WorkOS Admin Portal
// returns) and compares the tab with GET /v1/teams/{id}/identity. Opens no portal and changes
// nothing. Prints the steps (window.__checkSteps).
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
  const before = document.cookie.match(/(?:^|;\s*)pb-workspace=([^;]+)/)?.[1] ?? "personal"
  try {
    const response = await fetch(`/api/v1/teams/${teamId}/identity`)
    const identity = (await response.json()).data
    pass("GET identity answers", response.ok, String(response.status))
    // Arriving from another workspace, ?team= switches to the team.
    document.cookie = "pb-workspace=personal; Path=/"
    window.next.router.push(`/team?team=${teamId}&tab=security`)
    const tab = await until(() => /Security/.test(text()) && (/Single sign-on|available from/.test(text()) ? text() : null))
    pass("?team=…&tab=security opens the team's Security tab", Boolean(tab))
    pass(
      "the workspace switched to the team",
      document.cookie.includes(`pb-workspace=${teamId}`),
    )
    if (!identity.eligible) {
      pass(
        "below the seat minimum it says from how many seats, with Add seats",
        text().includes(`available from ${identity.min_seats} seats`) &&
          [...main().querySelectorAll("a")].some((a) => a.textContent.trim() === "Add seats"),
        `${identity.seats} of ${identity.min_seats}`,
      )
    } else {
      const t = text()
      pass("Domains, Single sign-on and Directory sync sections show", /Domains/.test(t) && /Single sign-on/.test(t) && /Directory sync/.test(t))
      pass(
        "every verified domain is listed",
        identity.sso.domains.every((domain) => t.includes(domain.domain)),
        identity.sso.domains.map((d) => d.domain).join(", "),
      )
      const required = document.querySelector('[aria-label="Require SSO"]')
      pass(
        "Require SSO shows the server's setting",
        required && (required.getAttribute("aria-checked") === "true") === identity.sso.required,
      )
      pass(
        "the portal buttons are offered to whoever may manage",
        identity.can_manage === [...main().querySelectorAll("button")].some((b) => /Set up SSO|Edit SSO/.test(b.textContent)),
      )
      pass(
        "directory groups are listed with their roles",
        identity.directory.groups.every((group) => t.includes(group.name)),
        `${identity.directory.groups.length} groups`,
      )
    }
  } finally {
    document.cookie = `pb-workspace=${before}; Path=/`
  }
  return steps
})()
