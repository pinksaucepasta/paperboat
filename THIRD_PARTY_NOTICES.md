# Third-party notices

Paperboat's own licenses are in [LICENSE](LICENSE). This file lists third-party code and assets
that ship inside the console and the site, beyond what a package manager installs and licenses
by itself. Go dependency licenses are checked by `cli/tools/verify-licenses.sh` and
`server/tools/verify-licenses.sh`; the Tailscale-derived sources are in
[cli/upstream/NOTICE.md](cli/upstream/NOTICE.md).

| What | Where it ships | License | Source |
|---|---|---|---|
| Ghostty VT core, built as WebAssembly (`ghostty-vt-0.5.1.wasm`, identical to the `@wterm/ghostty` 0.5.1 package file) | `console/public/terminal/` | Ghostty: MIT. `@wterm/ghostty` package: Apache-2.0 | <https://ghostty.org>, <https://github.com/vercel-labs/wterm> |
| `@wterm/dom` 0.5.1 with a resize fix | `console/patches/`, applied at install | Apache-2.0 | <https://github.com/vercel-labs/wterm> |
| `wasm_exec.js` (Go runtime shim for WebAssembly, copied from the Go toolchain by the build scripts, not committed) | `console/public/terminal/`, `console/public/environment-vault/` | BSD-3-Clause (The Go Authors) | <https://go.dev> |
| Phosphor icons (`@phosphor-icons/react`; SVGs from `@phosphor-icons/core` inlined by the site) | console, marketing | MIT | <https://github.com/phosphor-icons> |
| Figtree, Geist, JetBrains Mono (variable fonts: self-hosted from `@fontsource-variable/*` on the site, fetched at build by `next/font/google` in the console) | marketing, console | SIL Open Font License 1.1 | <https://fontsource.org>, <https://fonts.google.com> |
| shadcn/ui component sources (generated into `console/components/ui`) and Base UI | console | MIT | <https://github.com/shadcn-ui/ui>, <https://github.com/mui/base-ui> |
| BBRv1 congestion sender, adapted from Hysteria (`core/internal/congestion/bbr`), itself a port of Chromium's QUIC BBR | `third_party/quic-go/internal/congestion/bbr/` (linked into `pb`, relay and edge) | MIT (Hysteria); BSD-3-Clause (Chromium) | <https://github.com/apernet/hysteria>; full texts in `third_party/quic-go/internal/congestion/bbr/NOTICE.md` |

When a new vendored file or asset is added, add its row here in the same commit.
