// Browser check for W-10 / W-11: IP rules and the flood-protection notice on tunnel and preview pages.
// Run with run-console-check.mjs on a tunnel's page (/tunnels/<id>) after setting
//   __preview = "<preview id>"  (optional: also checks /previews/<id>)
// Saves a rule through the chip inputs, reloads and sees it, shows the bad-entry error, restores the rules,
// and compares the flood notice with GET /v1/tunnels/<id>/status. Prints the steps.
;(async () => {
  const preview = window.__preview
  delete window.__preview
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
  const main = () => document.querySelector("main main") ?? document.querySelector("main")
  const button = (label) => [...main().querySelectorAll("button")].find((b) => b.textContent.trim() === label)
  const type = (input, text) => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(input, text)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  }
  const field = (name) => document.getElementById(`ip-rules-${name}`)
  const chips = (name) =>
    [...field(name).parentElement.querySelectorAll("[data-slot=badge]")].map((b) => b.textContent.trim())
  const enter = (input) => input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }))
  const getJson = async (path) => (await (await fetch(`/api/v1/${path}`)).json()).data

  async function rules(kind, id) {
    const base = `${kind}s/${id}/ip-rules`
    const original = await getJson(base)
    const ready = await until(() => field("allow") && field("deny"))
    pass(`${kind}: IP rules section shows Allow and Deny`, Boolean(ready))
    if (!ready) return
    pass(`${kind}: chips match the server`, JSON.stringify(chips("allow")) === JSON.stringify(original.allow) && JSON.stringify(chips("deny")) === JSON.stringify(original.deny))

    // Bad entry: the server's 400 names it.
    type(field("deny"), "not-an-address")
    enter(field("deny"))
    await sleep(300)
    button("Save").click()
    const refused = await until(() => /not-an-address/.test(main().innerText) && /not an IP address/.test(main().innerText))
    pass(`${kind}: invalid entry is refused with its name`, Boolean(refused))
    const stored = await getJson(base)
    pass(`${kind}: refused entry saved nothing`, JSON.stringify(stored) === JSON.stringify(original))

    // Remove the bad chip, add a good Allow and Deny, one save.
    ;[...field("deny").parentElement.querySelectorAll("button[aria-label^=Remove]")]
      .find((b) => b.getAttribute("aria-label") === "Remove not-an-address")
      ?.click()
    await sleep(300)
    type(field("allow"), "203.0.113.0/24")
    enter(field("allow"))
    await sleep(200)
    type(field("deny"), "198.51.100.7")
    enter(field("deny"))
    await sleep(300)
    button("Save").click()
    const saved = await until(async () => {
      const now = await getJson(base)
      return now.allow.includes("203.0.113.0/24") && now.deny.includes("198.51.100.7") ? now : null
    })
    pass(`${kind}: both lists saved in one go`, Boolean(saved))

    location.reload()
    await sleep(6000)
    await until(() => field("allow"))
    pass(`${kind}: reload shows the saved chips`, chips("allow").includes("203.0.113.0/24") && chips("deny").includes("198.51.100.7"))

    // Restore.
    const restore = await fetch(`/api/v1/${base}`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(original),
    })
    pass(`${kind}: rules restored`, restore.ok)
  }

  const tunnel = location.pathname.match(/^\/tunnels\/([^/]+)/)?.[1]
  if (!tunnel) {
    pass("start on /tunnels/<id>", false, location.pathname)
    return steps
  }
  const flood = (await getJson(`tunnels/${tunnel}/status`)).flood_protection
  await sleep(4000)
  const notice = main().querySelector("[data-flood-protection]")
  if (flood) {
    pass("tunnel: flood notice shows count and summary", Boolean(notice) && notice.innerText.includes(String(flood.refused)) && notice.innerText.includes(flood.summary))
  } else pass("tunnel: no flood notice without refusals", !notice)
  await rules("tunnel", decodeURIComponent(tunnel))

  if (preview) {
    history.pushState(null, "", `/previews/${preview}`)
    location.reload()
    await sleep(8000)
    pass("preview: detail page shows its address and Details", /Details/.test(main().innerText) && /Forwards to/.test(main().innerText))
    await rules("preview", preview)
  }
  return steps
})()
