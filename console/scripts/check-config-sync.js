// Browser check for F11-05 (console Config sync screens). READ ONLY: paste into the console of a
// signed-in tab on /config (the workspace whose machines have config sync). Compares the all-machines
// table with `GET /v1/config-sync/status` (machine, repository, direction, files, status wording,
// last synced), then opens the machine page of the most interesting row (conflicts first, then
// waiting for approval, then the first) and compares its details and its Conflicts / Waiting for
// approval / Skipped lists with the server. It clicks only table links and Refresh; it never
// resolves, approves, pulls or pushes anything (those are `pb config` commands on the machine).
// Prints the steps (window.__checkSteps).
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
      await sleep(300)
    }
    return null
  }
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      try {
        return (await (await fetch(`/api/v1/${path}`, { signal: AbortSignal.timeout(8000) })).json()).data
      } catch (error) {
        if (attempt >= 4) throw error
        await sleep(600)
      }
    }
  }
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const direction = {
    bidirectional: "Two-way",
    pull_only: "From the repository",
    push_only: "To the repository",
    disabled: "Off",
  }
  // The wording the console promises for a state; the server's raw state decides which one applies.
  const expectedStatus = (e) => {
    const conflicts = e.conflicts?.length ?? 0
    if (conflicts) return `${conflicts} ${conflicts === 1 ? "conflict" : "conflicts"}`
    if (e.review?.length) return "Waiting for approval"
    return (
      {
        healthy: "In sync",
        watching: "In sync",
        syncing: "Syncing",
        restoring: "Syncing",
        pending: "Syncing",
        offline: "Offline",
        disabled: "Off",
        consent_required: "Needs consent on the machine",
        revoked: "Access revoked",
        warning: "Warning",
        sync_uncertain: "Couldn’t confirm the last sync",
      }[e.state] ?? null
    )
  }

  const status = await get("config-sync/status?limit=200")
  const environments = status?.environments ?? []
  pass("the server reports config sync", Boolean(status), `${environments.length} machines`)
  if (!environments.length) {
    pass("the page says config sync is not set up", /not set up|Set up|No machine/i.test(main()?.innerText ?? ""))
    return steps
  }
  const table = await until(() => main()?.querySelector("table"))
  pass("the page lists the machines in a table", Boolean(table))
  if (!table) return steps
  const rows = [...table.querySelectorAll("tbody tr")]
  pass("one row per machine", rows.length === environments.length, `${rows.length} of ${environments.length}`)
  for (const environment of environments) {
    const row = rows.find((r) => r.querySelector("td a")?.textContent.trim() === environment.alias)
    const cells = row ? [...row.querySelectorAll("td")].map((c) => c.innerText.trim()) : []
    const wanted = expectedStatus(environment)
    const ok =
      Boolean(row) &&
      cells[1] === (environment.repository_name ?? "—") &&
      cells[2] === (direction[environment.mode ?? ""] ?? "—") &&
      cells[3] === String(environment.managed_path_count) &&
      (wanted ? cells[4] === wanted : cells[4].length > 0) &&
      (environment.last_successful_sync_at ? cells[5] !== "—" : cells[5] === "—")
    pass(`row for ${environment.alias} matches the server`, ok, ok ? `${cells[4]}` : JSON.stringify(cells))
  }

  // Refresh re-reads the server without changing anything.
  const refresh = [...document.querySelectorAll("button")].find((b) => b.textContent.trim() === "Refresh")
  if (refresh) {
    refresh.click()
    pass("Refresh is available and the page stays", Boolean(await until(() => main()?.querySelector("table"))))
  }

  // Open the most interesting machine.
  const pick =
    environments.find((e) => e.conflicts?.length) ??
    environments.find((e) => e.review?.length) ??
    environments.find((e) => e.state !== "disabled") ??
    environments[0]
  const link = rows.find((r) => r.querySelector("td a")?.textContent.trim() === pick.alias)?.querySelector("td a")
  link?.click()
  // A machine page keeps its tabs in the inner main, so read the outer one.
  const page = () => document.querySelector("main")
  const opened = await until(() => page()?.innerText.includes("Details") && page().innerText.includes("Files synced"))
  pass(`the machine page for ${pick.alias} opens`, Boolean(opened))
  if (!opened) return steps
  const text = page().innerText
  const detail = (label) => {
    const index = text.indexOf(label)
    return index < 0
      ? ""
      : text
          .slice(index + label.length)
          .split("\n")
          .filter(Boolean)[0]
          ?.trim()
  }
  pass("details show the repository", pick.repository_name ? detail("Repository") === pick.repository_name : true)
  pass("details show the direction", detail("Direction") === (direction[pick.mode ?? ""] ?? ""))
  pass("details show the file count", detail("Files synced") === String(pick.managed_path_count))
  pass(
    "details show how updates apply",
    detail("Updates") === (pick.automatic_updates ? "Automatic" : "After approval"),
  )
  for (const [title, paths] of [
    ["Conflicts", pick.conflicts ?? []],
    ["Waiting for approval", pick.review ?? []],
    ["Skipped", pick.skipped ?? []],
  ]) {
    const listed = paths.length
      ? paths.every((p) => text.includes(p.path)) && text.includes(title)
      : !text.includes(`\n${title}\n`)
    pass(`${title} list matches the server`, listed, `${paths.length} paths`)
  }
  pass("the page points to pb config on the machine, not buttons here", !/Resolve|Force pull|Force push/.test(text))
  return steps
})()
