// Browser check of every console page (F2-07 / S2 quality pass). Paste into the console of a
// signed-in tab on any console page of the workspace to check (select it with the pb-workspace
// cookie first). READ ONLY: it navigates with the app router, never clicks buttons and never
// posts. For each route it records and flags:
//   - console errors, uncaught errors and unhandled rejections while the page loaded,
//   - dead links (internal links that 404, "#" links without a target),
//   - text a person should never see: "undefined", "null", "NaN", "[object Object]", snake_case
//     error codes, "HTTP 5xx", "Failed to fetch" and similar,
//   - empty states ("No …", "Nothing …", "Not set up") without a button or link to act on,
//   - API calls that failed (>= 400, except 401/404 probes) whose page shows a raw code or no
//     human sentence at all.
// `window.__only` limits the run to a list of routes; `window.__fail` (503 or "network") makes every
// API read fail to see what people are told. `window.__machine` (default: the first online machine) picks the machine whose tabs are visited;
// `window.__tunnel` the tunnel. Findings are in window.__findings (also returned); progress in
// window.__checkSteps. Nothing is left changed.
;(async () => {
  const findings = []
  const steps = []
  window.__findings = findings
  window.__checkSteps = steps
  const flag = (route, kind, detail) => findings.push({ route, kind, detail: String(detail).slice(0, 240) })
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
  const inject = window.__fail
  delete window.__fail
  const machineId = window.__machine
  const tunnelId = window.__tunnel
  delete window.__machine
  delete window.__tunnel

  // Hooks: console errors and failed API calls, reset for every route.
  let errors = []
  let failures = []
  const realError = console.error.bind(console)
  console.error = (...args) => {
    errors.push(args.map((a) => (a instanceof Error ? a.message : String(a))).join(" "))
    realError(...args)
  }
  const onError = (event) => errors.push(`uncaught: ${event.message}`)
  const onRejection = (event) => errors.push(`rejection: ${event.reason?.message ?? event.reason}`)
  window.addEventListener("error", onError)
  window.addEventListener("unhandledrejection", onRejection)
  const realFetch = window.fetch
  window.fetch = async (input, init) => {
    const url = typeof input === "string" ? input : input.url
    // Failure mode (window.__fail = 503 or "network"): every API read fails, to see what people see.
    const method = (init?.method ?? (typeof input === "string" ? "GET" : input.method) ?? "GET").toUpperCase()
    if (inject && method === "GET" && /\/api\/v1\//.test(url) && !/\/api\/v1\/me(\?|$)/.test(url)) {
      if (inject === "network") {
        failures.push({ url: url.replace(/^.*\/api\/v1\//, ""), status: 0, code: "Failed to fetch" })
        throw new TypeError("Failed to fetch")
      }
      failures.push({ url: url.replace(/^.*\/api\/v1\//, ""), status: inject, code: "internal_error" })
      return new Response(
        JSON.stringify({
          error: { code: "internal_error", message: "Paperboat hit a problem. Try again in a moment." },
        }),
        {
          status: inject,
          headers: { "content-type": "application/json" },
        },
      )
    }
    try {
      const response = await realFetch(input, init)
      if (response.status >= 400 && /\/api\/v1\//.test(url) && ![401, 404].includes(response.status)) {
        let code = ""
        try {
          const body = await response.clone().json()
          code = body?.error?.code ?? ""
        } catch {}
        failures.push({ url: url.replace(/^.*\/api\/v1\//, ""), status: response.status, code })
      }
      return response
    } catch (error) {
      if (/\/api\/v1\//.test(url) && !init?.signal?.aborted) failures.push({ url, status: 0, code: error.message })
      throw error
    }
  }
  const restore = () => {
    console.error = realError
    window.fetch = realFetch
    window.removeEventListener("error", onError)
    window.removeEventListener("unhandledrejection", onRejection)
  }

  const idPrefix =
    /^(machine|tunnel|preview|user|session|team|domain|route|connector|environment|operation|request|support|file|edge|op|evt|inv)_/
  const hidden = "code, pre, input, textarea, script, style, [class*=font-mono], [data-allow-raw], svg"
  const visibleText = () => {
    const root = document.querySelector("main")
    const out = []
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT)
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent.trim()
      if (!text || node.parentElement.closest(hidden)) continue
      out.push(text)
    }
    return out
  }
  const rawPatterns = [
    [/\bundefined\b/, "undefined"],
    [/\bnull\b/, "null"],
    [/\bNaN\b/, "NaN"],
    [/\[object Object\]/, "[object Object]"],
    [/\b(HTTP|status) ?[45]\d\d\b/i, "HTTP status"],
    [
      /Failed to fetch|NetworkError|Unexpected token|TypeError|SyntaxError|Internal Server Error|AbortError/,
      "technical error",
    ],
  ]
  const snake = /\b[a-z]{2,}(?:_[a-z0-9]+)+\b/g

  // A skeleton counts only when it can be seen: hidden sections keep theirs mounted.
  const busy = () =>
    [...document.querySelectorAll("main .animate-pulse, main [aria-busy=true]")].some(
      (e) => e.getClientRects().length > 0,
    )
  const loaded = async () => {
    await until(() => !busy(), 20000)
    await sleep(1200)
  }
  const visited = new Set()
  const links = new Set()
  const visit = async (route) => {
    errors = []
    failures = []
    window.next.router.push(route)
    const arrived = await until(
      () => location.pathname === route || (route !== "/" && location.pathname.startsWith(route)),
      30000,
    )
    if (!arrived) {
      flag(route, "navigation", `stayed on ${location.pathname}`)
      return
    }
    await loaded()
    if (inject && busy()) flag(route, "still loading after the failure", "a skeleton never went away")
    visited.add(route)
    steps.push({ step: `visited ${route}`, ok: true, detail: location.pathname })
    for (const message of errors) flag(route, "console error", message)
    const texts = visibleText()
    for (const text of texts) {
      for (const [pattern, name] of rawPatterns) if (pattern.test(text)) flag(route, `raw text (${name})`, text)
      for (const code of text.match(snake) ?? [])
        if (!idPrefix.test(code)) flag(route, "raw text (code)", `${code} in "${text}"`)
    }
    // Empty states: a "No …" line must come with something to do.
    for (const element of document.querySelectorAll("main p, main div, main span")) {
      if (element.children.length > 0 || element.closest(hidden)) continue
      const text = element.textContent.trim()
      if (text.length > 200 || !/^(No |Nothing |You (have|don’t have) no|Not set up|There (are|is) no)/i.test(text))
        continue
      let box = element
      let actionable = false
      for (let level = 0; level < 4 && box.parentElement && !actionable; level++) {
        box = box.parentElement
        actionable = [...box.querySelectorAll("a[href], button")].some(
          (b) => !b.closest("[role=tablist], nav") && !b.disabled,
        )
      }
      if (!actionable) flag(route, "empty state without a next action", text)
    }
    // Failed calls must read as something a person can act on.
    const page = texts.join(" ")
    for (const failure of failures) {
      // The machine list also feeds the header picker; a page that never mentions machines owes no sentence about it.
      if (
        /^machines\?/.test(failure.url) &&
        !/machine/i.test((document.querySelector("main main") ?? document.querySelector("main")).innerText)
      )
        continue
      const human =
        /Couldn’t|Couldn't|Could not|Try again|isn’t|offline|unavailable|didn’t|failed|expired|not available|went wrong|Retry/i.test(
          page,
        )
      if (failure.code && page.includes(failure.code))
        flag(route, "API error code shown", `${failure.status} ${failure.code} from ${failure.url}`)
      else if (!human)
        flag(route, "failed call without a visible message", `${failure.status} ${failure.code} ${failure.url}`)
    }
    for (const anchor of document.querySelectorAll("a[href]")) {
      const href = anchor.getAttribute("href")
      if (href === "#" || href === "" || /^javascript:/i.test(href))
        flag(route, "dead link", `"${anchor.textContent.trim().slice(0, 40)}" -> ${href}`)
      else if (href.startsWith("/") && !href.startsWith("//")) links.add(href.split("#")[0])
    }
  }

  let machine = machineId
  if (!machine) {
    const list = (
      await (
        await realFetch(
          "/api/v1/machines?workspace=" + (document.cookie.match(/pb-workspace=([^;]*)/)?.[1] ?? "personal"),
        )
      ).json()
    ).data
    machine = list?.items?.find((m) => m.online)?.id ?? list?.items?.[0]?.id
  }
  const routes = [
    "/",
    "/network",
    "/tunnels",
    "/previews",
    "/domains",
    "/files",
    "/terminals",
    "/environment",
    "/config",
    "/team",
    "/billing",
    "/usage",
    "/activity",
    "/account",
  ]
  if (machine) {
    routes.push(`/machines/${machine}`)
    for (const tab of ["terminals", "previews", "tunnels", "files", "environment", "config", "usage", "activity"])
      routes.push(`/machines/${machine}/${tab}`)
  }
  if (tunnelId) routes.push(`/tunnels/${tunnelId}`)
  routes.push("/cli/authorize")
  const only = window.__only
  delete window.__only
  for (const route of only ?? routes) {
    try {
      await visit(route)
    } catch (error) {
      flag(route, "check error", error.message)
    }
  }

  // Dead links: every internal link met while visiting must exist. Links that differ only by an ID
  // are one kind of link; two of each are checked. The tunnel to the console drops a request now
  // and then, so a failed request is retried before it counts.
  const perKind = new Map()
  const toCheck = [...links].filter((href) => {
    if (visited.has(href)) return false
    const kind = href.replace(/[a-z]+_[0-9a-f-]{20,}/g, ":id")
    perKind.set(kind, (perKind.get(kind) ?? 0) + 1)
    return perKind.get(kind) <= 2
  })
  for (const href of toCheck) {
    let status = 0
    let reason = ""
    for (let attempt = 0; attempt < 3 && !status; attempt++) {
      try {
        status = (await realFetch(href, { redirect: "follow", signal: AbortSignal.timeout(10000) })).status
      } catch (error) {
        reason = error.message
        await sleep(500)
      }
    }
    if (!status) flag("(links)", "dead link", `${href} -> ${reason}`)
    else if (status === 404) flag("(links)", "dead link", `${href} -> 404`)
  }
  steps.push({
    step: `checked ${visited.size} routes and ${toCheck.length} internal links`,
    ok: findings.length === 0,
    detail: `${findings.length} findings`,
  })
  restore()
  window.next.router.push("/")
  return findings
})()
