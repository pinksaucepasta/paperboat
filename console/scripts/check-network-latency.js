// Browser check for F9-06. Paste into the console of a signed-in tab on a machine's page
// (/machines/<id>). Reads the machine's measured latency from the API and compares it with the
// Network section: every edge and relay time the API returns must be on the page, and the page
// must show no time the API did not return (no sample data). Prints one row per check.
;(async () => {
  const id = location.pathname.match(/\/machines\/([^/]+)/)?.[1]
  const workspace = document.cookie.match(/pb-workspace=([^;]+)/)?.[1] ?? "personal"
  const steps = []
  const check = (step, ok, detail) => steps.push({ step, ok, detail })
  if (!id) return console.table([{ step: "on a machine page", ok: false, detail: location.pathname }])
  const response = await fetch(`/api/v1/machines/${id}/network-latency?workspace=${encodeURIComponent(workspace)}`)
  check("latency endpoint answers", response.ok, `HTTP ${response.status}`)
  const items = response.ok ? (await response.json()).data.items : []
  const section = [...document.querySelectorAll("section")].find((s) => s.querySelector("h2")?.innerText === "Network")
  check("Network section is on the page", Boolean(section), "")
  const shown = [...(section?.innerText.matchAll(/(\d+) ms/g) ?? [])].map((m) => Number(m[1])).sort((a, b) => a - b)
  const reported = items.map((item) => item.rtt_ms).sort((a, b) => a - b)
  check("every reported time is shown", reported.every((ms) => shown.includes(ms)), `API ${reported}, page ${shown}`)
  check("no time is shown that was not reported", shown.length === reported.length, `API ${reported}, page ${shown}`)
  check(
    "relays say so when none reported",
    items.some((i) => i.role === "relay") || /Not reported yet/.test(section?.innerText ?? ""),
    "",
  )
  console.table(steps)
  return steps.every((s) => s.ok)
})()
