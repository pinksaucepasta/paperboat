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
    const tab = await until(
      () => /Security/.test(text()) && (/Single sign-on|available from/.test(text()) ? text() : null),
    )
    pass("?team=…&tab=security opens the team's Security tab", Boolean(tab))
    pass("the workspace switched to the team", document.cookie.includes(`pb-workspace=${teamId}`))
    if (!identity.eligible) {
      pass(
        "below the seat minimum it says from how many seats, with Add seats",
        text().includes(`available from ${identity.min_seats} seats`) &&
          [...main().querySelectorAll("a")].some((a) => a.textContent.trim() === "Add seats"),
        `${identity.seats} of ${identity.min_seats}`,
      )
    } else {
      const t = text()
      pass(
        "Domains, Single sign-on and Directory sync sections show",
        /Domains/.test(t) && /Single sign-on/.test(t) && /Directory sync/.test(t),
      )
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
        identity.can_manage ===
          [...main().querySelectorAll("button")].some((b) => /Set up SSO|Edit SSO/.test(b.textContent)),
      )
      pass(
        "directory groups are listed with their roles",
        identity.directory.groups.every((group) => t.includes(group.name)),
        `${identity.directory.groups.length} groups`,
      )
      // Configured states (an identity provider on the team, e.g. lanes-sso).
      const switchOn = (label) =>
        document.querySelector(`[aria-label="${label}"]`)?.getAttribute("aria-checked") === "true"
      const providers = identity.sso.connections.map((c) => c.type.replace(/([a-z])([A-Z])/g, "$1 $2"))
      pass(
        "each SSO connection's provider is named",
        providers.every((name) => t.includes(name)),
        providers.join(", "),
      )
      pass("Join on first sign-in shows the server's setting", switchOn("Join on first sign-in") === identity.sso.jit)
      const exempt = identity.sso.exempt.map((e) => e.email ?? e.account_id)
      pass(
        "the exempt members are the server's",
        exempt.every((who) => t.includes(who)),
        exempt.join(", ") || "none",
      )
      const directory = identity.directory.directory
      if (directory) {
        pass(
          "the directory shows its type and how many people",
          t.includes(`${directory.type} · ${identity.directory.users} people`),
        )
        pass(
          "Only people in mapped groups shows the server's setting",
          switchOn("Only people in mapped groups") === identity.directory.mapped_only,
        )
        const roleOf = (name) => {
          const label = [...main().querySelectorAll("*")].find(
            (e) => e.children.length === 0 && e.textContent.trim() === name,
          )
          let row = label
          for (let i = 0; i < 4 && row && !row.querySelector("select"); i++) row = row.parentElement
          return row?.querySelector("select")?.value
        }
        const roles = identity.directory.groups.map((g) => [g.name, g.role ?? "", roleOf(g.name)])
        pass(
          "each group's role is the server's",
          roles.every(([, want, shown]) => want === shown),
          roles.map(([n, w, s]) => `${n} ${w}/${s}`).join(", "),
        )
      }

      // Members: directory-managed people are marked; held invitations keep their role.
      const members = (await (await fetch(`/api/v1/teams/${teamId}`)).json()).data?.members ?? []
      window.next.router.push(`/team?team=${teamId}`)
      await until(() => /Invitations/.test(text()) && /Joined/.test(text()) && text())
      const rowOf = (needle) =>
        [...main().querySelectorAll("tr")].find((tr) => tr.innerText.includes(needle))?.innerText ?? ""
      const managed = members.filter((m) => m.directory_managed)
      pass(
        "directory-managed members are marked Managed by directory, others are not",
        members.every((m) => !m.email || /Managed by directory/.test(rowOf(m.email)) === Boolean(m.directory_managed)),
        managed.map((m) => m.email).join(", ") || "none",
      )
      const invitations = (await (await fetch(`/api/v1/teams/${teamId}/invitations`)).json()).data?.items ?? []
      const roleLabels = { member: "Member", billing: "Billing admin", admin: "Admin", auditor: "Auditor" }
      ;[...main().querySelectorAll("button,[role=tab],[role=radio]")]
        .find((b) => /^Invitations/.test(b.textContent.trim()))
        ?.click()
      await until(() => /Invited by/.test(text()))
      const pending = invitations.filter((i) => i.state === "pending")
      pass(
        "each pending invitation shows its role",
        pending.every((i) => rowOf(i.email).includes(roleLabels[i.role] ?? i.role)),
        pending.map((i) => `${i.email} ${i.role}`).join(", ") || "none",
      )
    }
  } finally {
    document.cookie = `pb-workspace=${before}; Path=/`
  }
  return steps
})()
