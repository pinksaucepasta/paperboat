// Browser check: pages render the same on the server and in the browser (no hydration errors).
// Paste into the console of a signed-in tab on the live console (next dev). Each page loads in a
// hidden iframe; the check reads the Next dev overlay's issue count and its text (hydration errors
// and duplicate React keys are reported there), and checks that the server HTML carries no
// clock-relative text ("… ago") that the browser would compute differently. window.__machine
// picks the machine page (default: the first machine in the list), window.__pages the pages.
// Prints the steps.
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const machines = (await (await fetch("/api/v1/machines?limit=200")).json()).data
  const machine = window.__machine ?? (machines.items ?? machines)[0]?.id
  delete window.__machine
  const pages = window.__pages ?? ["/", `/machines/${machine}`, "/terminals", "/activity", "/billing", "/usage", "/domains"]
  delete window.__pages
  for (const path of pages) {
    // The server's HTML: relative times would differ by the time the browser hydrates.
    const html = await (await fetch(path)).text()
    const relative = html.match(/(Last seen [^<"]*ago|\b\d+ (seconds?|minutes?|hours?|days?) ago)/)
    pass(`${path}: server HTML has no clock-relative text`, !relative, relative?.[0] ?? "")

    const frame = document.createElement("iframe")
    frame.style.cssText = "position:fixed;left:-10000px;width:1280px;height:800px"
    frame.src = path
    document.body.append(frame)
    await new Promise((resolve) => frame.addEventListener("load", resolve, { once: true }))
    await sleep(8000)
    const overlay = frame.contentDocument.querySelector("nextjs-portal")?.shadowRoot
    const count = Number(overlay?.querySelector("[data-issues-count]")?.textContent ?? 0)
    const text = overlay?.textContent ?? ""
    pass(`${path}: no issues in the dev overlay`, count === 0, count ? `${count} issue(s)` : "")
    pass(
      `${path}: no hydration or duplicate-key error`,
      !/Hydration failed|didn't match|two children with the same key/i.test(text),
    )
    frame.remove()
  }
  return steps
})()
