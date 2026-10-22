// Browser check for the quick terminal's dock. Paste into the console of a signed-in tab on
// /machines/<id> of an ONLINE machine you may open a terminal on (set window.__machine to its id,
// or run it on that machine's page). It opens the quick terminal, waits until it is connected, then
// switches the dock to the side, back to the bottom and to the side again, and checks that:
//   - the panel orientation really changed each time (the separator turns vertical <-> horizontal),
//   - no new WebSocket was opened and none was closed (the connection is the same one),
//   - the terminal's host element is the same DOM node (the terminal was not remounted),
//   - the "Reconnecting" / "Connecting" / "Catching up" status never appeared while docking.
// It restores the dock it found and closes the quick terminal (which ends nothing remote: the
// session stays, as for any quick terminal). Prints the steps (window.__checkSteps).
;(async () => {
  const steps = []
  const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
  window.__checkSteps = steps
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const until = async (test, ms = 30000) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      const found = await test()
      if (found) return found
      await sleep(200)
    }
    return null
  }
  const machine = window.__machine ?? location.pathname.match(/^\/machines\/([^/]+)/)?.[1]
  delete window.__machine
  if (!machine) return [{ step: "on a machine page", ok: false, detail: location.pathname }]

  // Count sockets from now on.
  const Native = window.WebSocket
  const sockets = { opened: 0, closed: 0 }
  window.WebSocket = new Proxy(Native, {
    construct(target, args) {
      const socket = new target(...args)
      sockets.opened++
      socket.addEventListener("close", () => sockets.closed++)
      return socket
    },
  })
  const busy = []
  const watcher = new MutationObserver(() => {
    const text = document.querySelector("[role=status][aria-live=polite]")?.textContent?.trim()
    if (text && /Reconnecting|Connecting|Catching up/.test(text)) busy.push(text)
  })
  watcher.observe(document.body, { childList: true, subtree: true, characterData: true })
  const click = (el) => el.dispatchEvent(new MouseEvent("click", { bubbles: true }))
  const button = (label) => document.querySelector(`button[aria-label="${label}"]`)
  const orientation = () => document.querySelector("[data-slot=resizable-handle]")?.getAttribute("aria-orientation")
  const host = () => document.querySelector(".terminal-host")
  const dockLabel = () => (button("Dock to side")?.getAttribute("aria-pressed") === "true" ? "side" : "bottom")

  try {
    // Open it the way the machine list does; the parameter is removed once handled.
    if (!host()) window.next.router.push(`/machines/${machine}?terminal=open`)
    const opened = await until(host)
    pass("the quick terminal opens", Boolean(opened))
    if (!opened) return steps
    // Connected: sockets opened and the status line has gone away.
    const settled = await until(
      () => sockets.opened > 0 && !document.querySelector("[role=status][aria-live=polite]") && sockets,
      45000,
    )
    pass("the terminal connects", Boolean(settled), `sockets opened ${sockets.opened}`)
    if (!settled) return steps
    await sleep(2000)
    const originalDock = dockLabel()
    const element = host()
    const before = { ...sockets }
    busy.length = 0

    const seen = []
    for (const target of ["side", "bottom", "side"]) {
      if (dockLabel() === target) continue
      click(button(target === "side" ? "Dock to side" : "Dock to bottom"))
      const turned = await until(() => dockLabel() === target && orientation() === (target === "side" ? "vertical" : "horizontal") && target, 5000)
      seen.push([target, orientation()])
      pass(`docking to the ${target} turns the panels`, Boolean(turned), `separator ${orientation()}`)
      await sleep(2500)
    }
    pass("the same terminal element is kept", host() === element)
    pass(
      "no WebSocket opened or closed while docking",
      sockets.opened === before.opened && sockets.closed === before.closed,
      `opened ${sockets.opened - before.opened}, closed ${sockets.closed - before.closed}`,
    )
    pass("no Reconnecting / Connecting / Catching up status appeared", busy.length === 0, [...new Set(busy)].join(", "))

    // Leave things as found.
    if (dockLabel() !== originalDock) click(button(originalDock === "side" ? "Dock to side" : "Dock to bottom"))
    await sleep(500)
    click(button("Close terminal"))
  } finally {
    watcher.disconnect()
    window.WebSocket = Native
  }
  return steps
})()
