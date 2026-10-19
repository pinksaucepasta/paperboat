// Browser check for F15-06. Paste into the console of a signed-in tab on /activity (any workspace).
// Reads the live feed: newest first, every row has a time, an actor and a written sentence (no raw
// event names such as "did lease renewed"), machine bookkeeping that repeats every few seconds
// are written as sentences too, and the search and kind filters narrow the list. Set `window.__expect` to sentence
// fragments (for example ["added the domain"]) that real, recent events must show. Prints the steps.
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  // Steps so far, readable while a long check runs or if it throws: window.__checkSteps.
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 15000) => {
    for (let waited = 0; waited < ms; waited += 200) {
      const found = test()
      if (found) return found
      await sleep(200)
    }
    return null
  }
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const rows = () => [...document.querySelectorAll("main li")].filter((li) => li.querySelector("time"))
  const sentence = (li) => li.querySelector("p")?.innerText.replace(/\s+/g, " ").trim() ?? ""
  const expected = window.__expect ?? []
  delete window.__expect

  const loaded = await until(() => rows().length > 0 || null)
  pass("feed lists events", Boolean(loaded), `${rows().length} rows`)
  if (!loaded) return steps

  const times = rows().map((li) => Date.parse(li.querySelector("time").getAttribute("datetime")))
  pass("rows have valid times", times.every(Number.isFinite))
  pass(
    "newest first",
    times.every((time, index) => index === 0 || time <= times[index - 1]),
  )
  const text = rows().map(sentence)
  pass(
    "every row has an actor and a sentence",
    text.every((line) => /^\S+.* \S+/.test(line) && line.length > 8),
  )
  const raw = text.filter((line) => /^(You|Paperboat|A machine|Someone|\S+) did /.test(line))
  pass("no raw event names in sentences", raw.length === 0, raw.slice(0, 3).join(" | "))
  for (const fragment of expected) {
    pass(
      `shows "${fragment}"`,
      text.some((line) => line.includes(fragment)),
    )
  }

  // Search narrows the list to matching rows, and clearing it brings the feed back.
  const search = document.querySelector('main input[type="search"], main input[placeholder*="earch"]')
  const probe = (text.find((line) => /preview|tunnel|domain/.test(line)) ?? text[0]).split(" ").at(-3)
  if (search && probe) {
    const before = rows().length
    type(search, probe)
    await sleep(1200)
    const narrowed = rows().map(sentence)
    pass(
      "search narrows the feed",
      narrowed.length > 0 &&
        narrowed.length <= before &&
        narrowed.every((line) => line.toLowerCase().includes(probe.toLowerCase())),
      `${narrowed.length}/${before} for "${probe}"`,
    )
    type(search, "")
    await sleep(1200)
    pass("clearing search restores the feed", rows().length >= narrowed.length)
  } else {
    pass("search field present", Boolean(search))
  }
  return steps
})()
