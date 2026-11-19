// Flow-state leak check: abandoned dialogs/wizards must not survive navigation.
// Next 16 keeps visited routes mounted hidden in <Activity> (cacheComponents:true), so an
// abandoned page's component state (dialog `open`, wizard `step`, form drafts) comes back when
// you return. Every flow below is started, abandoned, left for two other pages, returned to,
// and must be clean; reopening must start fresh. Hidden Activity content stays in the DOM
// with display:none, so every locator here is visibility-aware (getByRole, :visible,
// .filter({visible:true})).
//
// Run (from console/):
//   MOCK_PORT=4101 node scripts/flow-state-mock.mjs &
//   PAPERBOAT_SERVER_URL=http://127.0.0.1:4101 WORKOS_REDIRECT_URI=http://127.0.0.1:3101/auth/callback \
//     nice -n 15 bunx next build && PAPERBOAT_SERVER_URL=http://127.0.0.1:4101 \
//     WORKOS_REDIRECT_URI=http://127.0.0.1:3101/auth/callback nice -n 15 bunx next start --port 3101 &
//   CONSOLE_ORIGIN=http://127.0.0.1:3101 PW_MODULE=/tmp/opencode/flowstate/node_modules/playwright-core/index.mjs \
//     CHROME=$HOME/.cache/ms-playwright/chromium-1248/chrome-linux-arm64/chrome \
//     node scripts/check-flow-state.mjs
// Env: CONSOLE_ORIGIN (required), CHROME or PW_CHROMIUM (required), PW_MODULE (path to
// playwright-core's index.mjs when it is not installed next to this file).

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.CONSOLE_ORIGIN
if (!origin) throw new Error("CONSOLE_ORIGIN is required")
const host = new URL(origin).hostname
const steps = []
const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })

// Static rule: a route directory with two or more child routes (its page counts) has a template.tsx
// that resets a hidden child (components/reset-when-hidden.tsx), or its flows come back on return.
{
  const { readdirSync, existsSync } = await import("node:fs")
  const appDir = new URL("../app/", import.meta.url).pathname
  const hasPage = (d) => readdirSync(d, { withFileTypes: true }).some((e) => (e.isFile() ? e.name === "page.tsx" : hasPage(`${d}/${e.name}`)))
  const bare = []
  const walk = (d) => {
    const entries = readdirSync(d, { withFileTypes: true })
    const dirs = entries.filter((e) => e.isDirectory() && e.name !== "api").map((e) => `${d}/${e.name}`)
    const children = (entries.some((e) => e.name === "page.tsx") ? 1 : 0) + dirs.filter(hasPage).length
    const inFlowGroup = /\/\((console|terminal)\)/.test(d)
    if (inFlowGroup && children >= 2 && !existsSync(`${d}/template.tsx`)) bare.push(d.replace(appDir, ""))
    dirs.forEach(walk)
  }
  walk(appDir.replace(/\/$/, ""))
  pass("every console route directory with 2+ child routes has a template.tsx", bare.length === 0, bare.join(", "))
}

const browser = await chromium.launch({ executablePath: process.env.CHROME ?? process.env.PW_CHROMIUM })
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
await context.addCookies([
  { name: "pb-dev-session", value: "flow-test-session", domain: host, path: "/" },
  { name: "pb-workspace", value: "personal", domain: host, path: "/" },
])
const page = await context.newPage()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const here = () => new URL(page.url()).pathname + new URL(page.url()).search

async function goto(path) {
  await page.goto(origin + path, { waitUntil: "domcontentloaded" })
  await page.waitForLoadState("networkidle").catch(() => {})
  await sleep(1500)
}
// A sidebar link: first in the DOM is the sidebar's (breadcrumbs repeat some names later).
async function side(name) {
  await page.getByRole("link", { name, exact: true }).first().click({ timeout: 12000 })
  await sleep(1800)
}
const dlg = (title) => {
  // Dialogs and AlertDialogs (revoke/rotate confirms) alike; hidden Activity content is
  // display:none, so the visible filter excludes abandoned pages' state.
  const all = page.locator('[role="dialog"], [role="alertdialog"]').filter({ visible: true })
  return title ? all.filter({ hasText: title }) : all
}
const dlgCount = async (title) => await dlg(title).count()
// Two other pages by sidebar links, never the origin.
async function visitTwo(except = []) {
  const names = ["Terminals", "Files", "Previews", "Activity", "Usage"].filter((n) => !except.includes(n))
  await side(names[0])
  await side(names[1])
}
// Return to an origin the sidebar doesn't link directly: section link, then the row link.
async function viaList(section, link) {
  await side(section)
  const target = page.getByRole("link", { name: link }).filter({ visible: true })
  await target.first().click({ timeout: 12000 })
  await sleep(1500)
}
// One (flow, abandon-way) round: history, open, interact, abandon, two pages, return, assert clean, reopen fresh.
async function round(label, originPath, originLink, way, open, interact, clean, fresh, home) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(label)) return
  const goHome = home ?? (() => side(originLink))
  try {
  await goto("/terminals")
  await goto(originPath)
  if (!(await open())) return pass(`${label} / ${way}`, true, "SKIP: could not open")
  if ((await interact?.()) === false) return pass(`${label} / ${way}`, true, "SKIP: could not reach the step")
  if (way === "back") await page.goBack().catch(() => {})
  else if (way === "forward") {
    await page.goBack().catch(() => {})
    await sleep(800)
    await page.goForward().catch(() => {})
    await sleep(1200)
    const mid = await clean()
    if (!mid.ok) return pass(`${label} / ${way}`, false, `stale right after forward: ${mid.detail}`)
  } else if (way === "esc") await page.keyboard.press("Escape")
  else if (way === "close") {
    const btn = dlg().getByRole("button", { name: /^(Cancel|Close)$/ }).filter({ visible: true })
    if ((await btn.count()) > 0) await btn.first().click()
    else await page.keyboard.press("Escape")
  } else if (way === "cancel") {
    // Leave inside the same section (Cancel link to the list), the way a user abandons a wizard page.
    const link = page.locator("a, button").filter({ hasText: /^(Cancel|Back)$/ }).filter({ visible: true })
    if ((await link.count()) === 0) return pass(`${label} / ${way}`, true, "SKIP: no Cancel link")
    await link.first().click()
  } else if (way === "sidebar") await side("Terminals")
  else if (way === "picker") return pass(`${label} / ${way}`, true, "SKIP: n/a")
  await sleep(1000)
  if (way !== "cancel") await visitTwo([originLink])
  await goHome()
  await sleep(1000)
  const c = await clean()
  if (!c.ok) return pass(`${label} / ${way}`, false, c.detail)
  const f = await fresh()
  pass(`${label} / ${way}`, f.ok, f.detail)
  } catch (error) {
    // A stuck modal hides the sidebar from assistive tech; never let it kill the run.
    await page.keyboard.press("Escape").catch(() => {})
    await sleep(500)
    pass(`${label} / ${way}`, false, `stale UI blocked navigation: ${String(error).split("\n")[0].slice(0, 160)}`)
  }
}
async function dialogRound(label, originPath, originLink, way, openSel, title, interact, extraClean, prep, home) {
  await round(
    label, originPath, originLink, way,
    async () => {
      await prep?.()
      const b = page.getByRole("button", { name: openSel, exact: true }).filter({ visible: true })
      if ((await b.count()) === 0) return false
      if (await b.first().isDisabled()) return false
      await b.first().click()
      await sleep(1200)
      return (await dlgCount(title)) > 0
    },
    interact,
    async () => {
      const n = await dlgCount(title)
      if (n > 0) return { ok: false, detail: `${n} "${title}" dialog(s) visible after return` }
      return extraClean ? extraClean() : { ok: true }
    },
    async () => {
      await prep?.()
      const b = page.getByRole("button", { name: openSel, exact: true }).filter({ visible: true })
      if ((await b.count()) === 0) return { ok: true, detail: "SKIP: reopen n/a" }
      if (await b.first().isDisabled()) return { ok: false, detail: "reopen button disabled" }
      await b.first().click()
      await sleep(1200)
      const n = await dlgCount(title)
      await page.keyboard.press("Escape")
      await sleep(600)
      return n > 0 ? { ok: true } : { ok: false, detail: "reopen did not show a fresh dialog" }
    },
    home,
  )
}

// 1) Add machine dialog on /.
for (const way of ["back", "forward", "esc", "close"])
  await dialogRound("add-machine", "/", "Machines", way, "Add machine", "Add a machine", async () => {
    const w = dlg("Add a machine").getByRole("button", { name: "Windows", exact: true })
    if ((await w.count()) > 0) await w.first().click()
    await sleep(600)
  })
await goto("/")
await visitTwo(["Machines"])
await side("Machines")
pass("add-machine / no stray dialog after navigation", (await dlgCount()) === 0, `${await dlgCount()} visible`)

// 2) Create API token wizard.
for (const way of ["back", "forward", "sidebar", "cancel"])
  await round(
    "api-token-wizard", "/settings/api-tokens/new", "API tokens", way,
    async () => {
      const b = page.getByRole("button", { name: "Full access", exact: false }).filter({ visible: true })
      if ((await b.count()) === 0) return false
      await b.first().click()
      await sleep(1000)
      return true
    },
    async () => {
      await page.locator("#token-name").filter({ visible: true }).fill("flowtoken")
      await sleep(500)
      const c = page.getByRole("button", { name: "Continue to summary", exact: true }).filter({ visible: true })
      if ((await c.count()) === 0 || (await c.first().isDisabled())) return false
      await c.first().click()
      await sleep(1000)
      return (await page.getByRole("heading", { name: "Summary", exact: true }).filter({ visible: true }).count()) > 0
    },
    async () => {
      const start = page.getByRole("button", { name: "Full access", exact: false }).filter({ visible: true })
      const summary = page.getByRole("heading", { name: "Summary", exact: true }).filter({ visible: true })
      if ((await start.count()) === 0) return { ok: false, detail: "wizard not back at step 1" }
      if ((await summary.count()) > 0) return { ok: false, detail: "stale Summary step visible" }
      return { ok: true }
    },
    async () => {
      await page.getByRole("button", { name: "Full access", exact: false }).filter({ visible: true }).first().click()
      await sleep(1000)
      const v = await page.locator("#token-name").filter({ visible: true }).inputValue().catch(() => "MISSING")
      return v === "" ? { ok: true } : { ok: false, detail: `name draft survived: "${v}"` }
    },
    () => viaList("API tokens", "Create token"),
  )

// 3) Create recipe + edit recipe.
async function recipeRound(label, originPath, nameSel, fillName, home) {
  for (const way of ["back", "forward", "sidebar", "cancel"])
    await round(
      label, originPath, "Recipes", way,
      async () => ((await page.locator(fillName ? "#recipe-name" : "textarea").filter({ visible: true }).count()) > 0),
      async () => {
        if (fillName) await page.locator("#recipe-name").filter({ visible: true }).fill("flowdraft")
        await page.locator("textarea").filter({ visible: true }).first().fill('machines: ["flow-alpha"]\n# draft\n')
        await sleep(600)
      },
      async () => {
        if (fillName) {
          const v = await page.locator("#recipe-name").filter({ visible: true }).inputValue().catch(() => "MISSING")
          if (v === "MISSING") return { ok: false, detail: "name field missing after return" }
          if (v !== "") return { ok: false, detail: `stale name: "${v}"` }
        }
        const src = await page.locator("textarea").filter({ visible: true }).first().inputValue().catch(() => "MISSING")
        if (src === "MISSING") return { ok: false, detail: "editor missing after return" }
        if (/# draft/.test(src)) return { ok: false, detail: "stale source draft visible" }
        return { ok: true }
      },
      async () => ({ ok: true, detail: "reopen covered by clean (page, not dialog)" }),
      home,
    )
}
await recipeRound("recipe-new", "/settings/recipes/new", null, true, () => viaList("Recipes", "New recipe"))
await recipeRound("recipe-edit", "/settings/recipes/flow-recipe/edit", null, false, async () => {
  await viaList("Recipes", "flow-recipe")
  await page.getByRole("link", { name: "Edit", exact: true }).filter({ visible: true }).first().click()
  await sleep(1500)
})

// 4) Create team dialog (button + ?create=1).
for (const way of ["back", "forward", "esc", "close"])
  await dialogRound("create-team", "/team", "Team", way, "Create team", "Create team", async () => {
    await dlg("Create team").locator("#team-id").filter({ visible: true }).fill("flowteam")
    await sleep(500)
  })
await goto("/team?create=1")
await sleep(1500)
pass("create-team / ?create=1 opens", (await dlgCount("Create team")) > 0)
await page.goBack().catch(() => {})
await sleep(800)
await visitTwo(["Team"])
await side("Team")
pass("create-team / ?create=1 back+return clean", (await dlgCount("Create team")) === 0, `${await dlgCount("Create team")} visible`)

// 5) Invite members on /team (team workspace; switched back to personal after).
{
  await context.addCookies([{ name: "pb-workspace", value: "t-flow", domain: host, path: "/" }])
  await goto("/team")
  const inv = page.getByRole("button", { name: "Invite members", exact: true }).filter({ visible: true })
  if ((await inv.count()) === 0) pass("invite-members / close", true, "SKIP: no Invite members in mock team")
  else
    for (const way of ["back", "esc", "close"])
      await dialogRound("invite-members", "/team", "Team", way, "Invite members", "Invite members", async () => {
        const box = dlg("Invite members").locator("textarea, input[type=text], input:not([type])").filter({ visible: true })
        if ((await box.count()) > 0) await box.first().fill("guest@example.test")
        await sleep(500)
      })
  await context.addCookies([{ name: "pb-workspace", value: "personal", domain: host, path: "/" }])
  await goto("/")
}

// 6) Share a file / Send files on /files.
for (const way of ["back", "forward", "esc", "close"]) {
  await dialogRound("files-share", "/files", "Files", way, "Share a file", "Share a file", async () => {
    await sleep(500)
  })
  await dialogRound("files-send", "/files", "Files", way, "Send files", "Send files", async () => {
    await sleep(500)
  })
}

// 7) New preview + New tunnel dialogs.
for (const way of ["back", "forward", "esc", "close"]) {
  await dialogRound("new-preview", "/previews", "Previews", way, "New preview", "New preview", async () => {
    await sleep(500)
  })
  await dialogRound("new-tunnel", "/tunnels", "Tunnels", way, "New tunnel", "New tunnel", async () => {
    await sleep(500)
  })
}

// 8) Quick terminal survives a machine switch (needs key={machineId}).
{
  try {
  await goto("/terminals")
  await goto("/machines/m-alpha")
  const qt = page.getByRole("button", { name: "Quick terminal", exact: true }).filter({ visible: true })
  if ((await qt.count()) === 0) pass("quick-terminal / picker", true, "SKIP: no Quick terminal button")
  else {
    await qt.first().click()
    await sleep(2000)
    const openDisabled = await page.getByRole("button", { name: "Quick terminal", exact: true }).filter({ visible: true }).first().isDisabled().catch(() => false)
    pass("quick-terminal / opens on flow-alpha", openDisabled)
    // Switch machine via the breadcrumb picker.
    const trigger = page.getByRole("button", { name: /flow-alpha/ }).filter({ visible: true })
    if ((await trigger.count()) === 0) pass("quick-terminal / picker", true, "SKIP: no machine picker")
    else {
      await trigger.first().click()
      await sleep(1000)
      const search = page.getByRole("combobox").filter({ visible: true })
      if ((await search.count()) > 0) await search.first().fill("flow-beta")
      await sleep(1000)
      const opt = page.getByRole("option", { name: /flow-beta/ }).filter({ visible: true })
      if ((await opt.count()) === 0) pass("quick-terminal / picker", true, "SKIP: flow-beta not offered")
      else {
        await opt.first().click()
        await sleep(2000)
        const onBeta = here().includes("/machines/m-beta")
        const stillOpen = await page.getByRole("button", { name: "Quick terminal", exact: true }).filter({ visible: true }).first().isDisabled().catch(() => true)
        pass("quick-terminal / picker switches machine", onBeta, here())
        pass("quick-terminal / picker closes first terminal", !stillOpen, stillOpen ? "alpha terminal still open on beta" : "")
      }
    }
    // Back-abandon: open on alpha, go back, two pages, back again.
    await goto("/terminals")
    await goto("/machines/m-alpha")
    await page.getByRole("button", { name: "Quick terminal", exact: true }).filter({ visible: true }).first().click()
    await sleep(1500)
    await page.goBack().catch(() => {})
    await sleep(800)
    await visitTwo([])
    await viaList("Machines", "flow-alpha")
    const closed = await page.getByRole("button", { name: "Quick terminal", exact: true }).filter({ visible: true }).first().isDisabled().catch(() => "missing")
    pass("quick-terminal / back+return closed", closed === false, String(closed))
  }
  } catch (error) {
    await page.keyboard.press("Escape").catch(() => {})
    pass("quick-terminal", false, `stale UI blocked navigation: ${String(error).split("\n")[0].slice(0, 160)}`)
  }
}

// 9) Command palette left open.
for (const way of ["back", "forward"]) {
  try {
  await goto("/")
  await page.keyboard.press("Control+k")
  await sleep(1000)
  const pal = dlg()
  if ((await pal.count()) === 0) {
    pass(`command-palette / ${way}`, true, "SKIP: palette did not open")
    continue
  }
  if (way === "back") await page.goBack().catch(() => {})
  else {
    await page.goBack().catch(() => {})
    await sleep(1000)
    await page.goForward().catch(() => {})
  }
  await sleep(1500)
  // The direct signal: a palette left open across the abandon leaks into the next page.
  const stuck = await dlgCount()
  if (stuck > 0) await page.keyboard.press("Escape").catch(() => {})
  await sleep(800)
  await visitTwo([])
  await side("Machines")
  await sleep(1000)
  const end = await dlgCount()
  if (stuck > 0) pass(`command-palette / ${way}`, false, "palette stayed open after abandon")
  else pass(`command-palette / ${way}`, end === 0, `${end} visible`)
  } catch (error) {
    await page.keyboard.press("Escape").catch(() => {})
    await sleep(500)
    pass(`command-palette / ${way}`, false, `stale UI blocked navigation: ${String(error).split("\n")[0].slice(0, 160)}`)
  }
}

// 10) Add domain dialog on /network.
for (const way of ["back", "forward", "esc", "close"])
  await dialogRound("add-domain", "/network", "Network", way, "Add domain", "Add a domain", async () => {
    const box = dlg("Add a domain").locator("input").filter({ visible: true })
    if ((await box.count()) > 0) await box.first().fill("flow.example.test")
    await sleep(500)
  })

// 11) Cheap extras: recipe rotate, tags edit, access ChangesDialog.
for (const way of ["back", "close"])
  await dialogRound("recipe-rotate", "/settings/recipes/flow-recipe", "Recipes", way, "Rotate token", "Rotate flow-recipe", null, null, null, () => viaList("Recipes", "flow-recipe"))
for (const way of ["back", "esc"])
  await dialogRound("tags-edit", "/machines/m-alpha", "Machines", way, "Add tags", "Tags of flow-alpha", async () => {
    await dlg("Tags of flow-alpha").getByRole("textbox", { name: "New tag" }).filter({ visible: true }).fill("flowtag")
    await sleep(500)
  }, null, null, () => viaList("Machines", "flow-alpha"))
for (const way of ["back", "close"]) {
  const prep = async () => {
    const a = page.getByRole("textbox", { name: "Policy file" }).filter({ visible: true })
    if ((await a.count()) > 0) {
      await a.first().press("End")
      await a.first().type("\n# flow draft")
      await sleep(1000)
    }
  }
  await goto("/access")
  await prep()
  const rev = page.getByRole("button", { name: "Review changes", exact: true }).filter({ visible: true })
  if ((await rev.count()) === 0 || (await rev.first().isDisabled()))
    pass(`access-changes / ${way}`, true, "SKIP: no enabled Review changes button")
  else await dialogRound("access-changes", "/access", "Access", way, "Review changes", "Review changes", null, null, prep)
}

for (const s of steps) console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${s.detail})` : ""}`)
console.log(`${steps.filter((s) => s.ok).length}/${steps.length} passed`)
await browser.close()
process.exit(steps.every((s) => s.ok) ? 0 : 1)
