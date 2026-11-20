# Signed release metadata (TUF) expiry

Every `pb`, edge and relay checks for updates against signed metadata. Each role has an expiry, and a client refuses
metadata past it, so a lapsed role stops all updates until it is re-signed. The server only serves the files; it
holds no signing key.

| Role | Lifetime | Re-signed by | Alert (admin bell + Overview "release_metadata") |
|---|---|---|---|
| timestamp | 24 h | the `TUF metadata refresh` workflow, every 6 h | under 12 h left |
| snapshot | 7 d | the same refresh | under 3 d left |
| targets | 90 d | every publish, rollout change, or `refresh-targets` | under 14 d left |
| root | about 2 years (v1 expires 2028-10-13) | offline root key ceremony | under 90 d left |

The limits live in `releases.ExpiryWarnBefore` (`server/internal/releases/expiry.go`). A warning is "degraded" on
Overview, "failed" once a role has expired; the bell shows one notice per role and expiry.

## Check

```sh
cd cli && PAPERBOAT_TUF_CI=1 go run ./tools/tuf-repository status -repository DIR
```

prints version and expiry of all four roles. Or open Overview in admin and read the `release_metadata` row.

## Timestamp or snapshot is close to expiry

The 6 h refresh is not running. Open the `TUF metadata refresh` run list in GitHub Actions, read why the last run
failed (online key secret, origin SSH, concurrency queue behind a long release), fix it and use "Run workflow". Run
by hand if Actions is down: `go run ./tools/tuf-repository refresh -repository DIR`, then `verify-published`, then
publish the directory to the origin the way the workflow's last steps do.

`refresh` stops when targets or root is expired: use the next section.

## Targets is close to expiry (or expired)

Quiet periods without a release never renew targets. Renew it without changing any target:

```sh
PAPERBOAT_TUF_ONLINE_KEY=... PAPERBOAT_TUF_CI=1 go run ./tools/tuf-repository refresh-targets -repository DIR
go run ./tools/tuf-repository verify-published -repository DIR
```

This bumps targets, snapshot and timestamp (90 d, 7 d, 24 h). It works on already-expired targets. Publish the
directory like a normal refresh, then check Overview turns healthy.

## Root is close to expiry, or the online key is lost or exposed

Only the offline root key (on the macOS release workstation) can fix this; the online key cannot. There is no
tool for it yet, so follow TUF's root rotation (TUF spec 5.3, "Update the root role"):

1. Write root version N+1 (`signed.version` N+1, new `expires`, and the new online key id for targets, snapshot and
   timestamp when the online key is being replaced).
2. Sign it with the root key of version N **and** with every root key version N+1 lists (threshold met in both).
3. Save it as `N+1.root.json` and also as `root.json` in the repository's `metadata/` directory.
4. Run `validate-signers -repository DIR -trusted-root shared/tufroot/trusted-root.json` (it walks every numbered
   root from the embedded one and checks the served root and the online key). Fix until it passes.
5. Put the new online key in the `PAPERBOAT_TUF_ONLINE_KEY` secret, then run `refresh-targets` (new root, new key).
6. Publish. Clients walk the numbered chain (up to 32 roots) from the root they ship with, so no client update is
   needed. Rotate at least once before the embedded root's expiry so fresh installs still verify the chain.

The offline root key never goes into CI. After a rotation, record the new root version and date in the incident
record. Never record key material.
