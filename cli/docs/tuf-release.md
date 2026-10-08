# TUF release operations

Paperboat supports five canonical native product assets. A release may publish any nonempty subset that has completed its platform qualification. Each is the complete unified `pb` executable or package for its platform and architecture:

- `pb-windows-amd64.exe`
- `pb-windows-arm64.exe`
- `pb-linux-amd64`
- `pb-linux-arm64`
- `pb-darwin-arm64.pkg`

Linux assets are raw ELF executables. Windows assets are PE executables. The macOS asset is an arm64 package containing one ad-hoc-signed executable in its `Library/PrivilegedHelperTools/Paperboat/bin/pb` payload path. The installer extracts that payload without installing the package, and the executable's public `install` command selects the invoking owner's canonical service path and links `~/.local/bin/pb`. Windows installation likewise selects the owner-scoped, SID-derived path rather than a machine-global executable. The installed executable handles CLI commands and explicit `pb daemon` service invocations.

Darwin verification requires a valid executable code signature and validates the package's publisher signature when present. Gatekeeper admission is not an updater authenticity gate; the updater retains TUF verification, executable format validation, and strict package payload checks without requiring a Gatekeeper override.

## Distribution contract

GitHub Releases hosts the selected assets from the five canonical products. It does not host a separate first-install verifier. The release origin serves only:

- the shell installer at `/install`
- the PowerShell installer selected by the PowerShell user agent
- signed TUF metadata under `/tuf/metadata/`

The origin's `tuf/targets/` directory must be empty. No release binary bytes are copied to the origin.

Each TUF target is one of the five canonical asset names. Its custom metadata has schema `paperboat.tuf-asset/v1`, kind `github-release-asset`, and includes:

- `version`, `platform`, `architecture`, and `format`
- `asset_name`, `repository`, immutable GitHub `url`
- `sha256` and `length`
- the signed `release_index` policy

For first installation, the trusted HTTPS shell or PowerShell installer pins the selected product's exact immutable GitHub URL, version, SHA-256, and length before execution. `pb install` validates the administrator-approved baseline bytes, format, and hash. This first download has no local TUF-signature verification. After installation, clients use the full TUF trust root and signed metadata to select release targets, check artifact length and SHA-256, activate updates, and enforce rollback protection.

On Unix, `pb update status` reports a recorded activation failure after recovery.
If the transaction or activation record cannot be read, it returns
`recovery_required` instead of reporting activation complete; completion remains
unknown until the updater can read its recovery state. The transaction preserves signed artifact identity and activation/recovery deadlines across helper restarts. macOS bootstrap verifies and extracts the package without modifying installed paths or receipts; the native installer transaction owns executable cutover. Completed Unix bootstrap binds the enrolled user daemon to the canonical installed executable; activation and rollback verify the running daemon and updater versions.

If an older activation helper cannot recover, a verified newer native reinstall can supersede its transaction. The updater verifies the installed executable against the signed payload and requires a strictly newer version before recording the new installation as idle and retiring the obsolete handoff. Recovery does not require editing the journal or replacing the helper by hand, and reinstall does not waive TUF verification or rollback protection.

The trusted HTTPS installer and its embedded product pin are the first-install trust anchor. A compromised installer origin could replace the product URL, version, hash, and length; first installation has no local TUF-signature verification. Once installed, full TUF verification protects future updates and rollback against compromise of GitHub asset or metadata hosting.

## Release sequence

1. Create and push a release tag.
2. The workflow runs the release checks and native platform tests.
3. It builds and verifies the selected assets from the five canonical products.
4. It creates or updates the GitHub release through the GitHub API, uploads the selected product assets, and verifies the API-reported size and digest.
5. The TUF signer updates the selected signed targets with their GitHub URLs and inline release policy. Omitted platforms retain their previously signed artifact identities, versions and policies.
6. The workflow renders both installers with the selected product's exact URL, version, SHA-256, and length, then stages them with TUF metadata for atomic activation on the server origin.

All pull requests run the reusable checks. The tag workflow repeats the small release contract checks and the required native checks before spending time on publication. No separate binary transfer or checksum-file handoff is part of the release.

## Installers

Users start with the trusted HTTPS installer:

`curl -fsSL https://get.pprbt.dev/install | sh`

Before first execution, the shell or PowerShell installer checks the pinned product URL, version, length, and SHA-256. Linux invokes the checked executable as `pb install --install-dir ABSOLUTE_DIRECTORY --json`. On macOS, the installer expands the checked package in its task-owned temporary directory and invokes the canonical payload at `Library/PrivilegedHelperTools/Paperboat/bin/pb install --json`; it does not run the package installer. On Windows, `pb install --json` owns UAC while preserving the invoking user. `pb install` validates the administrator-approved baseline bytes, format, and hash, then selects the owner-scoped absolute executable path returned in `data.executable`. First installation does not locally authenticate a TUF signature. Ordinary installation preserves enrollment and settings. A dashboard/token pairing securely stages the required token, then asks the installed binary to classify the protected resume journal. The same token resumes the existing partial pairing without reset or a replacement install. A new token runs confirmed `pb reset`, which synchronously removes Paperboat-owned services, configuration, credentials, keys, runtime state, and the prior installation while preserving Inbox payloads; any incomplete cleanup aborts before the token is consumed. It then installs and pairs through the returned installed binary without downloading a second artifact.

`pb install` installs the running executable without contacting a release server. Source/shared builds default to background update checks disabled. Official release builds enable periodic checks and notification; downloads and installation require user action, and every downloaded replacement still requires TUF verification. Enrollment contacts the account server independently and does not download another runtime.

## Windows qualification

`paperboat-tuf publish` requires one passed native qualification header for each Windows architecture selected for publication. The evidence binds the release version, Windows architecture, Windows build, runner, and `native_tested` status. It does not publish a separate executable or package target.

## Signing and maintenance

Keep TUF private keys out of the repository and runtime machines. Online role keys are protected GitHub environment secrets; root keys remain offline. The signer runs on the approved release workstation or in the explicitly authorized CI mode.

Use the signer for a qualified subset of the canonical assets:

First create and validate the canonical artifact manifest and signed deployment
policy from the exact selected files. The policy revision passed to the signer must
match the plan's `policy_revision`.

```sh
go run ./tools/release-plan manifest \
  -version YYYY.MM.DD.X \
  -source-commit <40-or-64-char-commit> \
  -toolchain go1.27.1 \
  -artifacts /absolute/path/to/qualified-assets \
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
  -artifacts /absolute/path/to/qualified-assets

paperboat-tuf publish \
  -repository /Users/pujan.pm/.local/share/paperboat-release/tuf-production \
  -version YYYY.MM.DD.X \
  -artifacts /absolute/path/to/qualified-assets \
  -manifest /absolute/path/to/manifest.json \
  -deployment-plan /absolute/path/to/deployment-plan.json \
  -windows-amd64-native-evidence /absolute/path/to/windows-amd64-native-qualification.json \
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

For the example above, select darwin-arm64, linux-amd64 and windows-amd64 assets; a selection including windows-arm64 also requires its native evidence file. Before publication, verify that the GitHub release contains every selected product asset, that each signed product URL points to that release, and that the origin's TUF target directory is empty.
