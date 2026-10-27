# TUF release operations

Paperboat publishes three canonical native product assets. The signer accepts any nonempty subset of them; the release workflow publishes all three. Each is the complete unified `pb` executable for its platform and architecture:

- `pb-linux-amd64`
- `pb-linux-arm64`
- `pb-darwin-arm64`

Linux assets are raw ELF executables. The macOS asset is a raw arm64 Mach-O executable, ad-hoc signed by the release runner. Windows is not published. The executable's public `install` command selects the invoking owner's canonical service path and links `~/.local/bin/pb`. The installed executable handles CLI commands and explicit `pb daemon` service invocations.

Darwin verification requires a valid executable code signature (`codesign --verify --deep --strict`). Gatekeeper admission is not an updater authenticity gate; the updater retains TUF verification and executable format validation without requiring a Gatekeeper override.

## Distribution contract

GitHub Releases hosts the three canonical products. It does not host a separate first-install verifier. The release origin serves only:

- the shell installer at `/install`
- signed TUF metadata under `/tuf/metadata/`

The origin's `tuf/targets/` directory must be empty. No release binary bytes are copied to the origin.

Each TUF target is one of the three canonical asset names. Its custom metadata has schema `paperboat.tuf-asset/v1`, kind `github-release-asset`, and includes:

- `version`, `platform`, `architecture`, and `format`
- `asset_name`, `repository`, immutable GitHub `url`
- `sha256` and `length`
- the signed `release_index` policy

For first installation, the trusted HTTPS shell installer pins the selected product's exact immutable GitHub URL, version, SHA-256, and length before execution. `pb install` validates the administrator-approved baseline bytes, format, and hash. This first download has no local TUF-signature verification. After installation, clients use the full TUF trust root and signed metadata to select release targets, check artifact length and SHA-256, activate updates, and enforce rollback protection.

On Unix, `pb update status` reports a recorded activation failure after recovery.
If the transaction or activation record cannot be read, it returns
`recovery_required` instead of reporting activation complete; completion remains
unknown until the updater can read its recovery state. The transaction preserves signed artifact identity and activation/recovery deadlines across helper restarts. The native installer transaction owns executable cutover. Completed Unix bootstrap binds the enrolled user daemon to the canonical installed executable; activation and rollback verify the running daemon and updater versions.

If an older activation helper cannot recover, a verified newer native reinstall can supersede its transaction. The updater verifies the installed executable against the signed payload and requires a strictly newer version before recording the new installation as idle and retiring the obsolete handoff. Recovery does not require editing the journal or replacing the helper by hand, and reinstall does not waive TUF verification or rollback protection.

The trusted HTTPS installer and its embedded product pin are the first-install trust anchor. A compromised installer origin could replace the product URL, version, hash, and length; first installation has no local TUF-signature verification. Once installed, full TUF verification protects future updates and rollback against compromise of GitHub asset or metadata hosting.

## Release sequence

1. Create and push a release tag.
2. The workflow validates the tag and source commit, the protected release endpoints and that the production metadata origin is readable, and that the deployed server already contains this release's server and shared commits (server first, then client).
3. It builds the three assets (Linux amd64 and arm64, macOS arm64) and smoke-tests each with `--version`.
4. It creates or resumes the GitHub release through the GitHub API, uploads the missing assets as a draft, and verifies the API-reported size and digest.
5. The TUF signer updates the signed targets with their GitHub URLs and inline release policy.
6. The workflow renders the shell installer with each product's exact URL, version, SHA-256, and length, then stages it with TUF metadata for atomic activation on the server origin. Only then is the release marked Latest.

The release workflow runs no test suite of its own: pull requests and `main` run the cross-platform and server checks, and the release builds are smoke-tested with `--version` before publication. No separate binary transfer or checksum-file handoff is part of the release.

## Installers

Users start with the trusted HTTPS installer:

`curl -fsSL https://get.pprbt.dev/install | sh`

Before first execution, the shell installer checks the pinned product URL, version, length, and SHA-256. Linux invokes the checked executable as `pb install --install-dir ABSOLUTE_DIRECTORY --json` (default `~/.local/bin`). On macOS, the installer invokes the checked executable as `pb install --json`; `--install-dir` is not supported there. `pb install` validates the administrator-approved baseline bytes, format, and hash, then selects the owner-scoped absolute executable path returned in `data.executable`. First installation does not locally authenticate a TUF signature. Ordinary installation preserves enrollment and settings. A dashboard/token pairing securely stages the required token, then asks the installed binary to classify the protected resume journal. The same token resumes the existing partial pairing without reset or a replacement install. A new token runs confirmed `pb reset`, which synchronously removes Paperboat-owned services, configuration, credentials, keys, runtime state, and the prior installation while preserving Inbox payloads; any incomplete cleanup aborts before the token is consumed. It then installs and pairs through the returned installed binary without downloading a second artifact.

`pb install` installs the running executable without contacting a release server. Source/shared (custom) builds cannot check for or install updates (`custom_installation`). Official installations enable scheduled automatic updates by default: releases are downloaded ahead of the daily maintenance time (04:00 on the machine's local clock) and installed then. `pb update settings --auto=false` keeps availability checks but leaves downloads and installation to the manual `pb update` commands. Every downloaded replacement still requires TUF verification. Enrollment contacts the account server independently and does not download another runtime.

## Signing and maintenance

Keep TUF private keys out of the repository and runtime machines. Online role keys are protected GitHub environment secrets; root keys remain offline. The signer runs on the approved release workstation or in the explicitly authorized CI mode.

Use the signer for the canonical assets:

First create and validate the canonical artifact manifest and signed deployment
policy from the exact selected files. The policy revision passed to the signer must
match the plan's `policy_revision`.

```sh
go run ./tools/release-plan manifest \
  -version YYYY.MM.DD.X \
  -source-commit <40-or-64-char-commit> \
  -toolchain go1.27.3 \
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
  -repository /absolute/path/to/tuf-production \
  -version YYYY.MM.DD.X \
  -artifacts /absolute/path/to/qualified-assets \
  -manifest /absolute/path/to/manifest.json \
  -deployment-plan /absolute/path/to/deployment-plan.json \
  -rollout-revision 1 \
  -severity routine

paperboat-tuf verify-published \
  -repository /absolute/path/to/tuf-production
```

`refresh`, `promote`, `pause`, and `quarantine` update signed TUF metadata only. Review and publish the complete metadata directory atomically. The origin remains metadata-only.

`promote`, `pause`, and `quarantine` mutate the single signed deployment policy
and require a strictly higher `-rollout-revision`. `promote` also requires
`-percentage 1..100`, raises every non-`general` cohort below that percentage to
it, sets `rollout_state=active` and is refused on a quarantined release; `pause` sets `paused`; and `quarantine`
sets `quarantined`. Automatic consumers are eligible only while the signed
state is active. The quarantine command does not use the release index's
cryptographic revocation flag.

Before publication, verify that the GitHub release contains every selected product asset, that each signed product URL points to that release, and that the origin's TUF target directory is empty.
