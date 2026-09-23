# TUF release operations

Paperboat publishes five native product assets. Each is the complete unified `pb` executable or package for its platform and architecture:

- `pb-windows-amd64.exe`
- `pb-windows-arm64.exe`
- `pb-linux-amd64`
- `pb-linux-arm64`
- `pb-darwin-arm64.pkg`

Linux assets are raw ELF executables. Windows assets are PE executables. The macOS asset is an arm64 package containing one ad-hoc-signed executable in its `Library/PrivilegedHelperTools/Paperboat/bin/pb` payload path. The bootstrap extracts that payload without installing the package, and the executable's public `install` command selects the invoking owner's canonical service path and links `~/.local/bin/pb`. Windows installation likewise selects the owner-scoped, SID-derived path rather than a machine-global executable. Publisher signing and notarization are optional; TUF authenticates every release asset. The installed executable handles CLI commands and explicit `pb daemon` service invocations.

## Distribution contract

GitHub Releases hosts those five product assets and five small bootstrap-verifier executables. The release origin serves only:

- the shell installer at `/install`
- the PowerShell installer selected by the PowerShell user agent
- signed TUF metadata under `/tuf/metadata/`

The origin's `tuf/targets/` directory must be empty. No release binary bytes are copied to the origin.

Each TUF target is one of the five canonical asset names. Its custom metadata has schema `paperboat.tuf-asset/v1`, kind `github-release-asset`, and includes:

- `version`, `platform`, `architecture`, and `format`
- `asset_name`, `repository`, immutable GitHub `url`
- `sha256` and `length`
- the signed `release_index` policy

Installed clients refresh and verify TUF metadata, select their canonical asset target, validate the custom metadata, and download the bytes from its immutable GitHub URL. They verify the downloaded length and SHA-256 against the TUF target before activating it. For first installation, the shell and PowerShell scripts download a release-pinned verifier from GitHub and check its length and SHA-256 before executing it. The verifier embeds the trusted TUF root, authenticates the current signed target, then downloads and verifies the product once from GitHub before the installer executes `pb`.

On Unix, `pb update status` reports a recorded activation failure after recovery.
If the transaction or activation record cannot be read, it returns
`recovery_required` instead of reporting activation complete; completion remains
unknown until the updater can read its recovery state. The transaction preserves the signed canary policy and activation/recovery deadlines across helper restarts. Canary policies require 2–32 samples. macOS bootstrap verifies and extracts the package without modifying installed paths or receipts; the native installer transaction owns executable cutover. Completed Unix bootstrap binds the enrolled user daemon to the canonical installed executable; activation and rollback verify the running daemon and updater versions.

If an older activation helper cannot recover, a verified newer native reinstall can supersede its transaction. The updater verifies the installed executable against the signed payload and requires a strictly newer version before recording the new installation as idle and retiring the obsolete handoff. Recovery does not require editing the journal or replacing the helper by hand, and reinstall does not waive TUF verification or rollback protection.

The installer script is the bootstrap trust anchor. A compromised script origin could replace its verifier pin; TUF protects the product against compromise of GitHub assets or signed-metadata storage when the installer itself is authentic. A first-time client has no prior TUF version state, so expiration bounds stale metadata but cannot prove absolute freshness.

## Release sequence

1. Create and push a release tag.
2. The workflow runs the release checks and native platform tests.
3. It builds and verifies the five product assets and five bootstrap-verifier assets.
4. It creates or updates the GitHub release through the GitHub API, uploads the five product and five bootstrap-verifier assets, and verifies the API-reported size and digest.
5. The TUF signer publishes five signed asset targets with the GitHub URLs and inline release policy.
6. The workflow renders both installers with verifier pins and stages them with TUF metadata for atomic activation on the server origin.

All pull requests run the reusable checks. The tag workflow repeats the small release contract checks and the required native checks before spending time on publication. No separate binary transfer or checksum-file handoff is part of the release.

## Installers

Users start with:

`curl -fsSL https://get.pprbt.dev/install | sh`

The shell installer verifies its pinned bootstrap executable, which selects the Linux or macOS asset through TUF and downloads that product once from GitHub before executing it. Linux invokes the verified executable as `pb install --install-dir ABSOLUTE_DIRECTORY --json`. On macOS, the bootstrap expands the verified package in its task-owned temporary directory and invokes the canonical payload at `Library/PrivilegedHelperTools/Paperboat/bin/pb install --json`; it does not run the package installer. The PowerShell bootstrap follows the same boundary with the verified Windows executable and lets `pb install --json` own UAC while preserving the invoking user. Each bootstrap validates the owner-scoped absolute executable path returned in `data.executable`. Ordinary installation preserves enrollment and settings. A dashboard/token pairing securely stages the required token, then asks the verified binary to classify the protected resume journal. The same token resumes the existing partial pairing without reset or a replacement install. A new token runs confirmed `pb reset`, which synchronously removes Paperboat-owned services, configuration, credentials, keys, runtime state, and the prior installation while preserving Inbox payloads; any incomplete cleanup aborts before the token is consumed. It then installs and pairs through the returned installed binary without downloading a second artifact.

`pb install` installs the running executable without contacting a release server. Source/shared builds default to automatic updates disabled. Official release builds enable automatic updates, whose downloaded replacements still require TUF verification. Enrollment contacts the account server independently and does not download another runtime.

## Windows qualification

`paperboat-tuf publish` requires one passed native qualification header for each Windows architecture. The evidence binds the release version, Windows architecture, Windows build, runner, and `native_tested` status. It does not publish a separate executable or package target.

## Signing and maintenance

Keep TUF private keys out of the repository and runtime machines. Online role keys are protected GitHub environment secrets; root keys remain offline. The signer runs on the approved release workstation or in the explicitly authorized CI mode.

Use the signer for the current five-asset repository:

First create and validate the canonical artifact manifest and signed deployment
policy from the exact five files. The policy revision passed to the signer must
match the plan's `policy_revision`.

```sh
go run ./tools/release-plan manifest \
  -version YYYY.MM.DD.X \
  -source-commit <40-or-64-char-commit> \
  -toolchain go1.27.1 \
  -artifacts /absolute/path/to/five-assets \
  -output /absolute/path/to/manifest.json

go run ./tools/release-plan plan \
  -manifest /absolute/path/to/manifest.json \
  -policy-revision 1 \
  -severity routine \
  -cohort-seed release-YYYY.MM.DD.X \
  -output /absolute/path/to/deployment-plan.json

go run ./tools/release-plan validate \
  -manifest /absolute/path/to/manifest.json \
  -plan /absolute/path/to/deployment-plan.json \
  -artifacts /absolute/path/to/five-assets

paperboat-tuf publish \
  -repository /Users/pujan.pm/.local/share/paperboat-release/tuf-production \
  -version YYYY.MM.DD.X \
  -artifacts /absolute/path/to/five-assets \
  -manifest /absolute/path/to/manifest.json \
  -deployment-plan /absolute/path/to/deployment-plan.json \
  -windows-amd64-native-evidence /absolute/path/to/windows-amd64-native-qualification.json \
  -windows-arm64-native-evidence /absolute/path/to/windows-arm64-native-qualification.json \
  -rollout-revision 1 \
  -severity routine

paperboat-tuf verify-published \
  -repository /Users/pujan.pm/.local/share/paperboat-release/tuf-production
```

`refresh`, `promote`, `pause`, and `quarantine` update signed TUF metadata only. Review and publish the complete metadata directory atomically. The origin remains metadata-only.

`promote`, `pause`, and `quarantine` mutate the single signed deployment policy
and require a strictly higher policy revision. `promote` may widen eligible
cohorts and sets `rollout_state=active`; `pause` sets `paused`; and `quarantine`
sets `quarantined`. Automatic consumers are eligible only while the signed
state is active. The quarantine command does not use the release index's
cryptographic revocation flag.

Before publication, verify that the GitHub release contains the five product and five verifier assets, that each signed product URL points to that release, and that the origin's TUF target directory is empty.
