// Browser check for F13-03 (console side: the bandwidth cap is reached). Paste into the console of a
// signed-in tab on any console page (Personal workspace). It stubs the server's answers in the page
// only (nothing is sent or changed): the workspace's billing says the allowance is used up, and the
// first tunnel's health says bandwidth_exhausted. Then it checks that:
//   - every page shows the quota banner with the plan limit, what is paused, that direct access still
//     works, and links to Billing ("Add bandwidth", "Change plan"),
//   - the tunnel's status reads "Bandwidth used up" and its page explains it with the same links,
//   - a 409 bandwidth_quota_exhausted from the API reads as plain words, not the code.
// The stubbed state is only in this tab; reloading removes it. Prints the steps (window.__checkSteps).
;(async () => {
  // Steps are kept in sessionStorage too: the dev server can reload a page it compiles on first visit.
  const saved = JSON.parse(sessionStorage.getItem("__quotaSteps") ?? "[]")
  const steps = saved
  const pass = (step, ok, detail = "") => {
    steps.push({ step, ok, detail })
    sessionStorage.setItem("__quotaSteps", JSON.stringify(steps))
  }
  window.__checkSteps = steps
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
  sessionStorage.removeItem("__quotaSteps")
  steps.length = 0
  // Visit the pages once first, so a first compile cannot reload the page in the middle of the check.
  for (const route of ["/billing", "/usage", "/tunnels", "/"]) {
    window.next.router.push(route)
    await until(() => location.pathname === route, 15000)
    await sleep(2500)
  }
  const realFetch = window.fetch.bind(window)
  const json = (body, status = 200) =>
    new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })
  const tunnelsRes = await realFetch("/api/v1/tunnels?limit=50")
  const tunnels = ((await tunnelsRes.json()).data?.items ?? []).filter((t) => t.desired_state === "active")
  const tunnel = tunnels[0]
  const stubTunnel = (t) => ({ ...t, summary_code: "bandwidth_exhausted" })
  window.fetch = async (input, init) => {
    const url = typeof input === "string" ? input : String(input.url ?? input)
    const path = url.replace(/^.*\/api\/v1\//, "")
    const real = () => realFetch(input, init)
    if (/^billing\/workspaces\/personal$/.test(path)) {
      const body = await (await real()).json()
      const data = { ...body.data, allowance_bytes: 5_000_000_000, usage_bytes: 5_000_000_100 }
      return json({ ...body, data })
    }
    if (tunnel && new RegExp(`^tunnels/${tunnel.id}/status`).test(path)) {
      return json({
        data: {
          overall_code: "bandwidth_exhausted",
          summary: "Your workspace has used its bandwidth allowance, so traffic is not forwarded.",
          repair_action:
            "Add bandwidth or change plan on the Billing page; traffic resumes when the allowance is available.",
        },
      })
    }
    if (tunnel && new RegExp(`^tunnels/${tunnel.id}$`).test(path)) {
      const body = await (await real()).json()
      return json({ ...body, data: stubTunnel(body.data) })
    }
    if (/^tunnels\?/.test(path)) {
      const body = await (await real()).json()
      return json({ ...body, data: { ...body.data, items: (body.data.items ?? []).map(stubTunnel) } })
    }
    return real()
  }
  try {
    // The billing answer is cached for a minute; an "online" event after that refetches it.
    await sleep(62000)
    // A hidden tab ignores focus; going offline and online makes React Query refetch every stale query.
    window.dispatchEvent(new Event("offline"))
    await sleep(300)
    window.dispatchEvent(new Event("online"))
    const banner = await until(() => document.querySelector("[data-quota-banner]"), 25000)
    pass("the quota banner appears when the allowance is used up", Boolean(banner))
    const text = banner?.innerText.replace(/\s+/g, " ") ?? ""
    pass("it names the limit and the plan", /5(\.0)? GB/.test(text) && /\(\w+/.test(text), text.slice(0, 100))
    pass(
      "it says what is paused and that direct access works",
      /paused/.test(text) && /Direct machine-to-machine access still works/.test(text),
    )
    const links = [...(banner?.querySelectorAll("a") ?? [])].map(
      (a) => `${a.textContent.trim()}:${a.getAttribute("href")}`,
    )
    pass(
      "it links to Billing to add bandwidth or change plan",
      links.includes("Add bandwidth:/billing") && links.includes("Change plan:/billing"),
      links.join(" "),
    )

    window.next.router.push("/billing")
    pass(
      "the banner stays on another page",
      Boolean(await until(() => location.pathname === "/billing" && document.querySelector("[data-quota-banner]"))),
    )

    if (tunnel) {
      window.next.router.push(`/tunnels/${tunnel.id}`)
      const notice = await until(
        () =>
          /Bandwidth used up/.test(document.querySelector("main")?.innerText ?? "") &&
          document.body.innerText.includes("traffic is not forwarded"),
        25000,
      )
      pass("the tunnel reads Bandwidth used up and explains why", Boolean(notice))
      const tunnelLinks = [...document.querySelectorAll("main a:not([data-quota-banner] a)")].map(
        (a) => a.textContent.trim() + ":" + a.getAttribute("href"),
      )
      pass(
        "the tunnel page links to Billing",
        tunnelLinks.includes("Add bandwidth:/billing") && tunnelLinks.includes("Change plan:/billing"),
      )
    } else {
      pass("no active tunnel to check", true, "skipped")
    }

    // The same words when an API call is refused for the quota.
    window.fetch = async (input, init) => {
      const url = typeof input === "string" ? input : String(input.url ?? input)
      if (/\/api\/v1\/usage\?/.test(url))
        return json({ error: { code: "bandwidth_quota_exhausted", message: "bandwidth_quota_exhausted" } }, 409)
      return realFetch(input, init)
    }
    window.next.router.push("/usage")
    const said = await until(() => /hosted bandwidth is used up/.test(document.body.innerText), 25000)
    pass("a 409 bandwidth_quota_exhausted reads as plain words", Boolean(said))
    pass("and never shows the code", !/bandwidth_quota_exhausted/.test(document.body.innerText))
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
