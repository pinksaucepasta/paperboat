// Browser check for F13-06. Paste into the console of a signed-in console tab on /billing for the
// workspace to check (cookie pb-workspace). Prints the API values and the page text; the dates
// and totals must agree (dates shown in the browser's time zone).
(async () => {
  const workspace = document.cookie.match(/pb-workspace=([^;]+)/)?.[1] ?? "personal"
  const path = workspace === "personal" ? "personal" : `teams/${workspace}`
  const billing = (await (await fetch(`/api/v1/billing/workspaces/${path}`)).json()).data
  const now = new Date()
  const params = new URLSearchParams({
    start: new Date(now - 30 * 864e5).toISOString(),
    end: now.toISOString(),
    limit: "200",
    workspace,
  })
  const usage = (await (await fetch(`/api/v1/usage?${params}`)).json()).data
  const byPath = { direct: 0, relay: 0, edge: 0 }
  for (const p of usage.totals.by_path) byPath[p.mode] += p.upload_bytes + p.download_bytes
  return {
    resets: billing.allowance_period_end,
    renews: billing.period_end,
    allowance_bytes: billing.allowance_bytes,
    usage_bytes: billing.usage_bytes,
    byPath,
    page: document.querySelector("main").innerText.replace(/\s+/g, " "),
  }
})()
