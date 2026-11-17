# Paperboat site

The marketing site: Astro, static output, light theme only. Served from Cloudflare Workers
(`wrangler.jsonc`, static assets from `dist/`).

```sh
cp .env.example .env     # PAPERBOAT_API_URL, PAPERBOAT_SITE_URL, optional PostHog; names and meanings in the file
bun install
bun run dev              # dev server; PAPERBOAT_SITE_URL optional here
bun run build            # needs PAPERBOAT_SITE_URL
bun run check            # astro check (types)
bun run check:site       # after a build: titles, links, anchors, commands, claims against the API's configuration, share images, structured data
```

- Addresses and limits come from the control plane (`GET /v1/client-configuration`), never from copy in the pages.
- `checks/og.mjs` regenerates the share images in `public/og/` (one per indexable page); see its header for the Chromium it needs.
- `checks/report-abuse.mjs` drives the abuse report form in a browser against a stand-in API.
- Fonts and icons are vendored from npm packages; their licenses are in [`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).
- License: MIT (`LICENSE`). The console is AGPL-3.0.
