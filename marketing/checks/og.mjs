// Makes one share image (1200 x 630) per indexable page into public/og/, from the built site. The home page
// shows its headline over the console's Machines screen; every other page its section and headline.
// Run after a build, then build again:
//   bun run build && CHROME=/path/to/chrome node checks/og.mjs && bun run build
// Needs playwright-core (npm i --no-save playwright-core) and a Chromium binary in CHROME.
import { chromium } from "playwright-core"
import { readFileSync, mkdirSync } from "node:fs"

const root = new URL("../", import.meta.url).pathname
// Inlined, because a page set from a string may not load file:// fonts or images.
const font = (name) =>
  `data:font/woff2;base64,${readFileSync(`${root}node_modules/@fontsource-variable/${name}/files/${name}-latin-wght-normal.woff2`).toString("base64")}`
const consoleShot = `data:image/png;base64,${readFileSync(`${root}src/assets/console/machines.png`).toString("base64")}`
const boat =
  "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
const sitemap = readFileSync(`${root}dist/sitemap.xml`, "utf8")
const site = sitemap.match(/<loc>https?:\/\/([^/]+)/)[1]
const paths = [...sitemap.matchAll(/<loc>https?:\/\/[^/]+([^<]*)<\/loc>/g)].map((match) => match[1])
const unescape = (html) => html.replace(/<[^>]+>/g, "").replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&#39;|&apos;/g, "'").replace(/&quot;/g, '"')
const escape = (text) => text.replace(/&/g, "&amp;").replace(/</g, "&lt;")
const sections = { features: "Features", "use-cases": "Use cases", compare: "Compare", pricing: "Pricing", security: "Security", docs: "Docs" }

const style = `
  @font-face { font-family: F; src: url("${font("figtree")}"); font-weight: 300 900; }
  @font-face { font-family: G; src: url("${font("geist")}"); font-weight: 100 900; }
  * { box-sizing: border-box; margin: 0; }
  body { position: relative; width: 1200px; height: 630px; overflow: hidden; background: #fafaf9; color: #141414; font-family: G; }
  .lockup { display: flex; align-items: center; gap: 16px; font: 600 28px F; letter-spacing: -0.025em; }
  h1 { font-family: F; font-weight: 500; letter-spacing: -0.035em; line-height: 1.04; text-wrap: balance; }`
const lockup = `<div class="lockup"><svg viewBox="0 0 600 360" height="28" fill="#1d52eb"><path fill-rule="evenodd" d="${boat}"/></svg>paperboat</div>`

const home = (headline) => `<style>${style}
  .top { position: absolute; left: 80px; top: 64px; right: 80px; }
  h1 { margin-top: 40px; font-size: 64px; max-width: 900px; }
  .shot { position: absolute; left: 80px; top: 304px; width: 1376px; border-radius: 14px; overflow: hidden;
    box-shadow: 0 0 0 1px rgb(20 20 20 / 0.08), 0 30px 60px -30px rgb(20 20 20 / 0.25); transform: scale(0.82); transform-origin: top left; }
  .shot img { display: block; width: 1376px; }
</style>
<div class="top">${lockup}<h1>${escape(headline)}</h1></div>
<div class="shot"><img src="${consoleShot}"></div>`

const inner = (section, headline) => `<style>${style}
  body { display: flex; flex-direction: column; padding: 64px 80px; }
  .title { margin-top: auto; }
  .label { margin-bottom: 18px; font-size: 26px; color: #6b6b6b; }
  h1 { max-width: 1000px; font-size: ${headline.length > 40 ? 64 : 76}px; }
  .foot { display: flex; justify-content: space-between; align-items: center; margin-top: 56px; padding-top: 28px; border-top: 1px solid #e8e8e6; font-size: 24px; color: #6b6b6b; }
</style>
${lockup}
<div class="title">${section ? `<p class="label">${escape(section)}</p>` : ""}<h1>${escape(headline)}</h1></div>
<div class="foot"><span>Work on every machine as if it were this one</span><span>${escape(site)}</span></div>`

mkdirSync(`${root}public/og`, { recursive: true })
const browser = await chromium.launch({ executablePath: process.env.CHROME, args: ["--font-render-hinting=none"] })
const page = await browser.newPage({ viewport: { width: 1200, height: 630 } })
for (const path of paths) {
  const html = readFileSync(`${root}dist${path}index.html`, "utf8")
  const headline = unescape(html.match(/<h1[^>]*>([\s\S]*?)<\/h1>/)[1]).replace(/\s+/g, " ").trim()
  const first = path.split("/")[1]
  await page.setContent(`<!doctype html>${path === "/" ? home(headline) : inner(path.split("/").length > 3 ? sections[first] : "", headline)}`)
  await page.evaluate(() => Promise.all([document.fonts.ready, ...[...document.images].map((img) => img.decode())]))
  const name = path.replace(/^\/|\/$/g, "").replace(/\//g, "-") || "home"
  await page.screenshot({ path: `${root}public/og/${name}.png` })
}
await browser.close()
console.log(`og: ${paths.length} images`)
