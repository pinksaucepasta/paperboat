// Browser check for Files' workspace Inbox batches without 50+ real machines. Run in a signed-in
// tab on /files (e.g. a headless Chromium). It stubs GET /api/v1/inbox: the first batch reads 50
// machines (3 files, one offline machine) and names a next_machine_cursor; the second reads 20 more
// (2 files). Checks: rows come newest first, the note says how many machines were read, "Load more
// machines" adds the second batch's files and then goes away, and the offline machine is noted.
// Changes nothing. Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 40000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(300)
    }
    return null
  }
  const file = (n, machine, minutesAgo) => ({
    id: `f${n}`,
    name: `stub-file-${n}.txt`,
    size: 100 + n,
    received_at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
    machine: { id: `m${machine}`, alias: `stub-machine-${machine}` },
  })
  const batches = {
    "": {
      items: [file(1, 3, 5), file(2, 7, 1), file(3, 40, 30)],
      machines_checked: 50,
      files_on_checked_machines: 3,
      truncated: [],
      unavailable: [{ machine_id: "m99", alias: "stub-offline", reason: "machine_offline" }],
      next_machine_cursor: "c2",
    },
    c2: { items: [file(4, 55, 2), file(5, 60, 60)], machines_checked: 20, files_on_checked_machines: 2, truncated: [], unavailable: [] },
  }
  const realFetch = window.__realFetch ?? window.fetch
  window.__realFetch = realFetch
  window.fetch = async (input, init) => {
    const url = new URL(typeof input === "string" ? input : input.url, location.href)
    if (url.pathname !== "/api/v1/inbox") return realFetch(input, init)
    return Response.json({ data: batches[url.searchParams.get("machine_cursor") ?? ""] })
  }
  const main = () => [...document.querySelectorAll("main main")].find((e) => e.offsetParent !== null) ?? document.querySelector("main")
  const text = () => main().innerText.replace(/\s+/g, " ")
  const names = () => [...main().querySelectorAll("tbody tr")].map((row) => row.innerText.match(/stub-file-\d+/)?.[0]).filter(Boolean)
  try {
    await until(() => names().length === 3)
    pass("the first batch's files, newest first", names().join(",") === "stub-file-2,stub-file-1,stub-file-3", names().join(","))
    pass("it says how many machines were read", /first 50 machines/.test(text()))
    pass("the offline machine is noted", /stub-offline: machine offline, Inbox unavailable/.test(text()))
    ;[...main().querySelectorAll("button")].find((b) => b.textContent.trim() === "Load more machines")?.click()
    await until(() => names().length === 5, 15000)
    pass("Load more machines adds the next batch, merged newest first", names().join(",") === "stub-file-2,stub-file-4,stub-file-1,stub-file-3,stub-file-5", names().join(","))
    pass("no more to load after the last batch", ![...main().querySelectorAll("button")].some((b) => b.textContent.trim() === "Load more machines"))
  } finally {
    window.fetch = realFetch
  }
  return steps
})()
