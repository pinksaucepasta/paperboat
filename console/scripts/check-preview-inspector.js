// Browser check for F17-03..06. Paste into the console of a signed-in tab on
// /previews/<id>/inspect for a live preview you own (machine with the inspector capability).
// Set window.__previewHost to the preview's host to let the check send traffic to it.
// Checks: the page connects, capture can be turned on, requests arrive live without
// duplicates, filters narrow the list, the detail pane shows headers and bodies, replay
// appears marked "Replay", pause stops updates, and Clear empties the list. Leaves capture
// as it found it. Prints the steps; they stay readable in window.__checkSteps.
;(async () => {
  const host = window.__previewHost
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 20000) => {
    for (let waited = 0; waited < ms; waited += 250) {
      const found = await test()
      if (found) return found
      await sleep(250)
    }
    return null
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const section = () => document.querySelector("[data-inspector-phase]")
  const state = () => document.querySelector("[data-inspector-state]")?.getAttribute("data-inspector-state")
  const button = (label, root = document) =>
    [...root.querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const rows = () => [...document.querySelectorAll("[data-capture-id]")]
  const ids = () => rows().map((row) => row.getAttribute("data-capture-id"))
  const switchFor = (label) =>
    [...document.querySelectorAll("label")].find((l) => l.textContent.trim() === label)?.querySelector("[role=switch]")
  const hit = (path) => fetch(`https://${host}${path}`, { mode: "no-cors", cache: "no-store" }).catch(() => null)

  const ready = await until(() => section() || ["Not allowed", "Machine offline", "Machine needs an update", "Preview stopped"].includes(state()), 30000)
  pass("page connects or shows a clear state", Boolean(ready), state() ?? section()?.dataset.inspectorPhase ?? "")
  if (!section()) return console.table(steps), steps
  const capture = switchFor("Capture")
  const wasOn = capture?.getAttribute("aria-checked") === "true"
  if (capture && !wasOn) {
    capture.click()
    pass("capture turns on", Boolean(await until(() => section()?.dataset.capture === "on")))
  }
  // Replay needs bodies (raw capture) on before the traffic.
  const bodies = switchFor("Bodies")
  const bodiesWereOn = bodies?.getAttribute("aria-checked") === "true"
  if (bodies && !bodiesWereOn) {
    bodies.click()
    pass("bodies turn on", Boolean(await until(() => switchFor("Bodies")?.getAttribute("aria-checked") === "true")))
  }
  if (!host) {
    pass("window.__previewHost set (needed to send traffic)", false)
    return console.table(steps), steps
  }
  const tag = `pbcheck-${Date.now()}`
  await hit(`/${tag}/a`)
  await hit(`/${tag}/b?x=1`)
  const live = await until(() => rows().filter((row) => row.innerText.includes(tag)).length >= 2, 20000)
  pass("requests arrive live", Boolean(live))
  await sleep(3000)
  pass("no duplicate rows after several polls", new Set(ids()).size === ids().length, `${ids().length} rows`)

  const search = document.querySelector('input[placeholder="Filter by path"]')
  type(search, `${tag}/b`)
  await sleep(500)
  pass("path filter narrows the list", rows().length === 1 && rows()[0].innerText.includes(`${tag}/b`))
  type(search, "")
  button("5xx")?.click()
  await sleep(300)
  pass("status filter hides 2xx rows", rows().every((row) => /\b5\d\d\b/.test(row.innerText)))
  button("All")?.click()
  await sleep(300)

  const target = rows().find((row) => row.innerText.includes(`${tag}/a`))
  target?.click()
  const detail = await until(() => document.querySelector("[data-capture-detail]"))
  pass(
    "detail shows request and response headers",
    Boolean(detail) && /Request headers/.test(detail.innerText) && /Response/.test(detail.innerText),
  )
  const replay = detail && button("Replay", detail)
  if (replay) {
    const before = ids().length
    replay.click()
    const marked = await until(() => ids().length > before && rows().some((row) => /Replay/.test(row.innerText)), 20000)
    pass("replay appears in the list marked Replay", Boolean(marked))
  } else pass("replay button shown (needs the replay grant)", false)

  button("Pause")?.click()
  const paused = ids().length
  await hit(`/${tag}/paused`)
  await sleep(2500)
  pass("pause stops updates", ids().length === paused)
  button("Resume")?.click()
  pass("resume brings in the paused request", Boolean(await until(() => ids().length > paused)))

  button("Clear")?.click()
  const confirm = await until(() => document.querySelector("[role=alertdialog]"))
  if (confirm) button("Clear", confirm)?.click()
  pass("clear empties the list", Boolean(await until(() => state() === "No requests yet", 15000)))
  if (bodies && !bodiesWereOn) switchFor("Bodies")?.click()
  if (capture && !wasOn) switchFor("Capture")?.click()
  console.table(steps)
  return steps
})()
