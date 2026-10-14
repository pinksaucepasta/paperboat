# TUF repository publisher

The initial public root is signed offline on apple and committed at
`cli/internal/hostruntime/bootstrap/trusted-root.json`. Its private root key stays
on apple. Publication jobs never create or load an offline private key.

Set `PAPERBOAT_TUF_ONLINE_KEY` through the job secret environment: unpadded standard
base64 of the 32-byte Ed25519 seed. The root must authorize this same key, at
threshold one, for targets, snapshot and timestamp. CI jobs also set
`PAPERBOAT_TUF_CI=1`; workstation signing runs on macOS.

Initialize an absent repository directory using the approved public root:

```sh
go run ./tools/tuf-repository init -repository /absolute/repository \
  -trusted-root /absolute/paperboat/cli/internal/hostruntime/bootstrap/trusted-root.json
```

Use the existing `publish`, `refresh`, `promote`, `pause`, `quarantine`, `status`,
`validate-signers` and `verify-published` commands against that repository.
`validate-signers -repository DIR -trusted-root FILE` checks the complete numbered
root chain and the online key without changing repository metadata. No local
signing-state file, Keychain item or separate metadata repository is required.
