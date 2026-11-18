# brand

The Paperboat brand, in one place. The marketing site (`marketing/`) and the status page (`status/`) import it; nothing is copied.

| Path | What |
|---|---|
| `styles/brand.css` | the font imports, every design token (colour, type scale, spacing, radius), the base element styles, focus ring and `.skip` |
| `src/brand.ts` | `boatPath`, the mark on its 10 × 6 cell grid |
| `src/components/Mark.astro`, `Lockup.astro` | the mark, and the lockup (mark + wordmark; takes an `href`) |
| `public/` | `favicon.svg`, `favicon.ico`, `apple-touch-icon.png`; each site's `public/` links to these |

Packages in the repo's Bun workspace (`../package.json`): `brand`, `marketing`, `status`. One `bun install` at the repo root installs them all; the font
packages resolve from there. A change here changes both sites: build both before pushing.
