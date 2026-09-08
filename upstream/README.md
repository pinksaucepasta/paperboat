# Paperboat upstream foundations

This directory records the upstream networking inputs that the Paperboat transport work
is allowed to consume. It contains pins, notices, and the consumed Tailcat and Tailscale source snapshots
with the narrow integration patches recorded in `PATCHES.md`.

`foundations.tsv` records each source's origin, exact Git commit, Go module version,
license notice and declared Go version. The Tailcat pin is
`5a83b9f9e119aad6b558cbc122d94efdca87452d`; its `go.mod` pins the Tailscale module to
`31d8badb3bfb88618dc8ea8e6a5c3bce0cd6cc9f`. Those are a compatible pair. The local
checkouts in `references/` remain inspection evidence; a newer checkout is not silently
substituted for the version Tailcat declares.

The repository fields point at the public upstream origins. A Paperboat fork is not
required until a downstream patch must be distributed. If that happens, update the
origin and commit together, preserve the upstream base in `PATCHES.md`, and rerun the
bounded check. Never replace a commit pin with `main`, `latest`, or an unreviewed module
version.

Run from this repository's root:

```sh
make upstream-foundations
```

`upstream-foundations` is intentionally a local, bounded check. It validates manifest
shape, immutable commit identity in the existing reference checkouts, module/toolchain
facts, Tailcat's Tailscale dependency, and retained license notices. It does not fetch
or resolve modules, compile upstream applications, run their tests, or modify pins.
Task 6 owns actual module consumption and integration checks.

No Paperboat transport or authorization policy belongs in these upstream records; such
policy must stay behind the adapters owned by later tasks.

Task 11 consumes the pinned Tailscale module snapshot in `tailscale/` through the
module replacement. Its BSD-3-Clause license and other upstream notices are retained.
Only the carrier seam listed in `PATCHES.md` is maintained downstream; no upstream
application is built as part of this intake. The private relay is in the sibling
`paperboat-relay` module, consumed through a local module replacement until publication.
