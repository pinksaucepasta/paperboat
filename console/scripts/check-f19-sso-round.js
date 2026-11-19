// Browser check for F19-13's Require SSO round as an owner/admin (team in window.__team, default lanes-sso;
// window.__me is the signed-in account id). It reads the team's identity first and runs the path that team
// can take: exempts the signed-in account and removes the exemption again (always), and
//  - with an active identity-provider connection: requires SSO (the review lists who loses access), checks
//    Activity for the change, the exemption and the bypass, then turns SSO off again;
//  - without one (a throwaway team: a connection exists only after the WorkOS Admin Portal, with a real
//    identity provider): asserts that Require SSO is disabled and the server refuses it (sso_not_ready).
// Prints the steps.
;(async () => {
  const steps = []
  const pass = (s, o, d = "") => steps.push({ step: s, ok: o, detail: d })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
  const until = async (t, ms = 20000) => {
    const e = Date.now() + ms
    while (Date.now() < e) {
      const f = await t()
      if (f) return f
      await sleep(250)
    }
    return null
  }
  const me = window.__me
  const team = window.__team ?? "lanes-sso"
  delete window.__me
  delete window.__team
  const mine = async () => (await (await fetch("/api/v1/me")).json()).data
  const identity = async () => (await (await fetch(`/api/v1/teams/${team}/identity`)).json()).data
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const text = () => main()?.innerText.replace(/\s+/g, " ") ?? ""
  const sw = (l) => document.querySelector(`[aria-label="${l}"]`)
  const setSelect = (el, v) => {
    Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value").set.call(el, v)
    el.dispatchEvent(new Event("change", { bubbles: true }))
  }
  document.cookie = `pb-workspace=${team}; Path=/`
  window.next.router.push(`/team?team=${team}&tab=security`)
  // The tab shows skeletons until the identity has loaded.
  const settled = () => sw("Require SSO") && !main().querySelector('[data-slot="skeleton"]')
  await until(settled)
  const connected = (await identity()).sso.connections.some((c) => c.state === "active")
  try {
    setSelect(document.querySelector('[aria-label="Exempt a member"]'), me)
    const ex = await until(async () => (await identity()).sso.exempt.includes(me))
    pass("Exempt a member adds the exemption on the server", Boolean(ex))
    pass(
      "the exemption is listed with Remove",
      Boolean(await until(() => [...main().querySelectorAll("button")].some((b) => b.textContent.trim() === "Remove"))),
    )
    if (!connected) {
      const off = sw("Require SSO")
      pass(
        "without a connection Require SSO is off and disabled",
        off.getAttribute("aria-checked") === "false" && off.hasAttribute("data-disabled"),
      )
      const refused = await fetch(`/api/v1/teams/${team}/identity/sso`, {
        method: "PUT",
        headers: { "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
        body: JSON.stringify({ required: true }),
      })
      const body = await refused.json().catch(() => null)
      pass(
        "the server refuses Require SSO without a connection",
        refused.status === 409 && body?.error?.code === "sso_not_ready",
        `${refused.status} ${body?.error?.code}`,
      )
      pass("Require SSO is still off on the server", !(await identity()).sso.required)
      return steps
    }
    const email = (await mine()).email
    sw("Require SSO").click()
    const dlg = await until(() => document.querySelector("[role=alertdialog]"))
    const listed = dlg ? [...dlg.querySelectorAll("li")].map((l) => l.textContent.trim()) : []
    pass(
      "Require SSO first shows who would lose access",
      Boolean(dlg) && listed.length > 0 && !listed.some((e) => e.startsWith(email)),
      listed.join(", "),
    )
    ;[...(dlg?.querySelectorAll("button") ?? [])].find((b) => b.textContent.trim() === "Require SSO")?.click()
    const on = await until(async () => (await identity()).sso.required)
    pass("confirming requires SSO on the server", Boolean(on))
    pass(
      "the switch shows it on",
      (await until(() => sw("Require SSO")?.getAttribute("aria-checked") === "true")) === true,
    )
    const t = await fetch(`/api/v1/teams/${team}`)
    pass("the exempt admin still reaches the team", t.ok, String(t.status))
    const acts = (await (await fetch(`/api/v1/activity?workspace=${team}&limit=50`)).json()).data?.items ?? []
    const by = acts.filter((a) => (a.action ?? a.type) === "team.sso_bypassed")
    const req = acts.filter((a) => (a.action ?? a.type) === "team.sso_settings" && a.details?.required === true)
    pass(
      "Activity has the Require SSO change and the bypass",
      req.length > 0 && by.length > 0,
      `${req.length} sso_settings required, ${by.length} sso_bypassed`,
    )
    window.next.router.push("/activity")
    const shown = await until(
      () =>
        /used the team without SSO/.test(text()) &&
        /changed the team's SSO settings/.test(text()) &&
        /changed who is exempt from SSO/.test(text()),
      20000,
    )
    pass("Activity names the bypass, the SSO change and the exemption", Boolean(shown))
    window.next.router.push(`/team?team=${team}&tab=security`)
    await until(settled)
  } finally {
    if (sw("Require SSO")?.getAttribute("aria-checked") === "true") {
      sw("Require SSO").click()
    }
    const off = await until(async () => !(await identity()).sso.required)
    pass("turned off again", Boolean(off))
    ;[...main().querySelectorAll("button")].find((b) => b.textContent.trim() === "Remove")?.click()
    pass("exemption removed", Boolean(await until(async () => !(await identity()).sso.exempt.includes(me))))
    document.cookie = "pb-workspace=personal; Path=/"
  }
  return steps
})()
