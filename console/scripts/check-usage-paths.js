// Browser check for F13-02/F13-06. Paste into the console of a signed-in tab on /usage (7 days
// selected) for the workspace to check. Reads the API for the same window and compares the
// Direct / Relay / Edge cards with it, then compares billing "used" with relay + edge over the
// allowance period: direct bytes are shown but never charged. Prints one row per check.
;(async () => {
  const workspace = document.cookie.match(/pb-workspace=([^;]+)/)?.[1] ?? "personal"
  const get = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data
  const units = { B: 1, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12 }
  const parse = (text) => {
    const m = text.match(/([\d.]+)\s*(B|KB|MB|GB|TB)\b/)
    return m ? Number(m[1]) * units[m[2]] : NaN
  }
  const totals = async (start) => {
    const params = new URLSearchParams({ start, end: new Date().toISOString(), limit: "200", workspace })
    const usage = await get(`usage?${params}`)
    const by = { direct: 0, relay: 0, edge: 0 }
    for (const p of usage.totals.by_path) by[p.mode] += p.upload_bytes + p.download_bytes
    return by
  }
  const steps = []
  const check = (step, ok, detail) => steps.push({ step, ok, detail })

  const week = await totals(new Date(Date.now() - 7 * 864e5).toISOString())
  const cards = [...document.querySelectorAll("main div")].filter((d) => /^(Direct|Relay|Edge)\n/.test(d.innerText))
  for (const mode of ["direct", "relay", "edge"]) {
    const label = mode[0].toUpperCase() + mode.slice(1)
    const card = cards.find((c) => c.innerText.startsWith(label))
    const shown = card ? parse(card.innerText.split("\n")[1] ?? "") : NaN
    // The page rounds to three digits and its window ends a little earlier than this read.
    const ok = Math.abs(shown - week[mode]) <= Math.max(0.05 * week[mode], 1e3)
    check(`${label} card matches the API`, ok, `page ${shown} bytes, API ${week[mode]} bytes`)
  }
  const billing = await get(
    workspace === "personal" ? "billing/workspaces/personal" : `billing/workspaces/teams/${workspace}`,
  )
  const period = await totals(billing.allowance_period_start)
  check(
    "billing used equals relay + edge, not direct",
    billing.usage_bytes === period.relay + period.edge,
    `used ${billing.usage_bytes}, relay ${period.relay} + edge ${period.edge}, direct ${period.direct}`,
  )
  return steps
})()
