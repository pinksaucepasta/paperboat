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

The MCP client/server dependency is pinned to `github.com/modelcontextprotocol/go-sdk`
`v1.8.0` (`3f3b699b2b67e1ed033a63d6651671dab53c2d32`). Its license is transitioning
from MIT to Apache-2.0; code retains the applicable original MIT or Apache-2.0 terms.
Both are permissive, with notices retained independently of Paperboat's license; see
`licenses/mcp-go-sdk-LICENSE.txt`. No documentation from that dependency is copied.

The MCP dependency also brings `github.com/segmentio/asm` `v1.2.1`, licensed under
MIT No Attribution (MIT-0); see `licenses/segmentio-asm-LICENSE.txt`. The pinned
legacy license classifier does not recognize MIT-0; this notice does not exempt
that dependency from the repository license policy.

Reviewed classifier exceptions live in [license-exceptions.json](license-exceptions.json).
The verifier checks the selected module version and both the recorded and downloaded
license SHA256 before excluding classification; upgrades and replacements require review.
Paperboat's own packages are excluded while their third-party dependencies remain checked.
