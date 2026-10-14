# paperboat-console

The Paperboat web console: Next.js 16 (App Router, Cache Components), Bun, shadcn/ui on Base UI,
TanStack Query, nuqs.

```bash
bun install
bun run dev        # or: bun run build && bun run start
bun run lint       # ESLint (Next rules)
bun run format     # Prettier, with Tailwind class ordering; format:check to verify
bun run api:types  # regenerate lib/api-schema.d.ts from ../paperboat-server/docs/openapi.json
```

API types come from the server's OpenAPI document (`lib/api-schema.d.ts`, checked in, never
edited by hand); `lib/api.ts` reads response types from it with `Data<"/v1/…">`. Regenerate after
server contract changes.

`.env.local`:

| Name | Purpose |
| --- | --- |
| `PAPERBOAT_SERVER_URL` | Control plane origin |
| `WORKOS_CLIENT_ID` | WorkOS AuthKit client |
| `WORKOS_REDIRECT_URI` | `https://<console origin>/auth/callback`; also pins the console origin |

## How it fits together

- **Auth** — the control plane owns the WorkOS session. `/auth/sign-in` gets a signed state from
  `/v1/auth/workos/state` and redirects to AuthKit; `/auth/callback` exchanges the code at
  `/v1/auth/workos/callback`, which sets the session cookies; `/auth/sign-out` revokes it.
  `proxy.ts` sends visitors without a session cookie to sign-in.
- **Data** — browser code calls `/api/v1/*`, a same-origin relay to the control plane's `/v1/*`.
  The relay enforces the console origin on writes and attaches the CSRF token server-side; the
  control plane authenticates and authorizes every call.
- **Machine scope** — machine-scoped sections keep the selected machine in `?machine=<id>`, so links
  from a machine page (`/previews?machine=…`) and the header picker share one source of truth.

## Browser terminal

Terminals render with `@wterm/dom` on `@wterm/ghostty`, pinned to the same exact version;
`patches/` carries a resize fix for `@wterm/dom`. `public/terminal/` is the runtime, copied as
built and excluded from lint and formatting:

- `ghostty-vt-0.5.1.wasm` — Ghostty's VT core, identical to the package's `wasm/ghostty-vt.wasm`.
  Its versioned URL is cached for a year (`next.config.ts`), so updating the package means
  copying the new file under a new name.
- `browserterminal.wasm`, `wasm_exec.js`, `browser-terminal-worker.js`, `output-flow.js` — the
  end-to-end encrypted connection, built from `paperboat-oss/cli/cmd/browserterminal-wasm`.

Themes are CSS variables (`lib/terminal/themes.ts`); each viewer's choice is kept in their browser.

## Icons

Phosphor (`@phosphor-icons/react`) is the system set: navigation, controls, status and anything
shown next to other icons. Use `weight="regular"` by default and `weight="fill"` for selected
state. Modules that server components import use `@phosphor-icons/react/dist/ssr`.
Hugeicons or Heroicons may be added for one-off decorative art (empty states, illustrations)
where no other icon sits beside it for comparison; add the package when first used.
