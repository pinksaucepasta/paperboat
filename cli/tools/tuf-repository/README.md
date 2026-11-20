# TUF repository publisher

The initial public root is signed offline on macos-test and committed at
`shared/tufroot/trusted-root.json`. Its private root key stays
on macos-test. Publication jobs never create or load an offline private key.

Set `PAPERBOAT_TUF_ONLINE_KEY` through the job secret environment: unpadded standard
base64 of the 32-byte Ed25519 seed. The root must authorize this same key, at
threshold one, for targets, snapshot and timestamp. CI jobs also set
`PAPERBOAT_TUF_CI=1`; workstation signing runs on macOS.

Initialize an absent repository directory using the approved public root:

```sh
go run ./tools/tuf-repository init -repository /absolute/repository \
  -trusted-root /absolute/paperboat/shared/tufroot/trusted-root.json
```

Use the existing `publish`, `refresh`, `refresh-targets`, `promote`, `pause`, `quarantine`, `status`,
`validate-signers` and `verify-published` commands against that repository.
`validate-signers -repository DIR -trusted-root FILE` checks the complete numbered
root chain and the online key without changing repository metadata. No local
signing-state file, Keychain item or separate metadata repository is required.

`refresh-targets` renews the 90-day targets expiry (and snapshot and timestamp) without changing a target; `refresh`
cannot do that. Expiry alerts and root rotation: `server/docs/runbooks/release-metadata.md`.
