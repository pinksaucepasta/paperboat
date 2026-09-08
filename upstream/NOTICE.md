# Upstream notices

The foundation sources are fetched from the repositories and commits in
`foundations.tsv`. Their notices are retained under `licenses/` so a later
Paperboat fork or binary distribution does not lose the upstream attribution boundary.

| Source | Repository | Pinned commit | Notice |
| --- | --- | --- | --- |
| Tailcat | <https://github.com/tailscale/tailcat> | `5a83b9f9e119aad6b558cbc122d94efdca87452d` | BSD 3-Clause; see `licenses/tailcat-LICENSE.txt`. |
| Tailscale DERP | <https://github.com/tailscale/tailscale> | `31d8badb3bfb88618dc8ea8e6a5c3bce0cd6cc9f` | BSD 3-Clause plus Tailscale's additional patent grant; see `licenses/tailscale-LICENSE.txt` and `licenses/tailscale-PATENTS.txt`. |

These notices cover the upstream source boundary. Transitive Go module licenses remain
subject to the owning repository's license check after the modules are integrated.
