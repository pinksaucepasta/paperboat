// Makes one share image (1200 x 630) per indexable page into public/og/, from the built site:
// the page's headline and its first real command. Run after a build, then build again:
//   bun run build && CHROME=/path/to/chrome node checks/og.mjs && bun run build
// Needs playwright-core (npm i --no-save playwright-core) and a Chromium binary in CHROME.
import { chromium } from "playwright-core"
import { readFileSync, mkdirSync } from "node:fs"

const root = new URL("../", import.meta.url).pathname
// Inlined, because a page set from a string may not load file:// fonts.
const font = (name) =>
  `data:font/woff2;base64,${readFileSync(`${root}node_modules/@fontsource-variable/${name}/files/${name}-latin-wght-normal.woff2`).toString("base64")}`
const boat =
  "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
const sitemap = readFileSync(`${root}dist/sitemap.xml`, "utf8")
const paths = [...sitemap.matchAll(/<loc>https?:\/\/[^/]+([^<]*)<\/loc>/g)].map((match) => match[1])
const unescape = (html) => html.replace(/<[^>]+>/g, "").replace(/&amp;/g, "&").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&#39;|&apos;/g, "'").replace(/&quot;/g, '"')
const escape = (text) => text.replace(/&/g, "&amp;").replace(/</g, "&lt;")

mkdirSync(`${root}public/og`, { recursive: true })
const browser = await chromium.launch({ executablePath: process.env.CHROME })
const page = await browser.newPage({ viewport: { width: 1200, height: 630 } })
for (const path of paths) {
  const html = readFileSync(`${root}dist${path}index.html`, "utf8")
  const headline = unescape(html.match(/<h1[^>]*>([\s\S]*?)<\/h1>/)[1]).trim()
  const install = html.match(/data-copy="([^"]+)"/)?.[1] ?? ""
  const command = unescape(html.match(/<span class="p">\$<\/span> <span class="r">([\s\S]*?)<\/span>/)?.[1] ?? install)
  await page.setContent(`<!doctype html><style>
    @font-face { font-family: F; src: url("${font("figtree")}"); font-weight: 300 900; }
    @font-face { font-family: M; src: url("${font("jetbrains-mono")}"); font-weight: 100 800; }
    * { box-sizing: border-box; margin: 0; }
    body { width: 1200px; height: 630px; padding: 72px 80px; display: flex; flex-direction: column; background: #fff; color: #0b0b0c; font-family: F; }
    .lockup { display: flex; align-items: center; gap: 20px; font-size: 30px; font-weight: 600; letter-spacing: -0.025em; }
    h1 { margin-top: auto; max-width: 960px; font-size: ${headline.length > 44 ? 68 : 84}px; line-height: 1.04; font-weight: 600; letter-spacing: -0.04em; text-wrap: balance; }
    pre { margin-top: 48px; padding-top: 32px; border-top: 2px solid #e3e5e9; font: 500 26px M; font-variant-ligatures: none; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
    pre b { color: #1d52eb; font-weight: 500; }
  </style>
  <div class="lockup"><svg viewBox="0 0 600 360" height="30" fill="#1d52eb"><path fill-rule="evenodd" d="${boat}"/></svg>paperboat</div>
  <h1>${escape(headline)}</h1>
  <pre><b>$</b> ${escape(command)}</pre>`)
  await page.evaluate(() => document.fonts.ready)
  const name = path.replace(/^\/|\/$/g, "").replace(/\//g, "-") || "home"
  await page.screenshot({ path: `${root}public/og/${name}.png` })
}
await browser.close()
console.log(`og: ${paths.length} images`)
