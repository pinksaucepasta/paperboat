# Paperboat containers

`pb daemon --foreground` runs Paperboat as a container's main command (see
[operations](../../docs/operations.md#containers-pb-daemon---foreground)). It is one supervisor in Go,
the only process manager: it checks the state tree, enrolls the machine on the first start from
`PAPERBOAT_ENROLLMENT_CREDENTIAL` (a token from `pb machine add`) and runs `paperboat hostd` as the
`paperboat` user (uid/gid `10001`). It also does what a service install gives hostd: it publishes the
root-owned session owner descriptor under `/var/lib/paperboat/host`, runs the session owner as a child
and serves hostd's start request for it with the updater's code. Hostd owns managed SSH channels, file
transfers, previews and live relay or direct streams; the session owner holds the terminal PTYs. There is no in-container updater: a new version is a new
image or binary and a restart.

Any Linux container works: install `pb` with the one-liner (on a machine without systemd it only
copies the binary) and make `pb daemon --foreground` the main command. The supplied image does the
same with `tini` as PID 1 to reap orphaned processes.

The `/var/lib/paperboat` volume is mandatory and holds two ownership domains:

| Path | Owner | Purpose |
| --- | --- | --- |
| `runtime/` and `hostd/` | `paperboat` UID/GID `10001`, mode `0700` | enrollment, durable workload state and the hostd capability token |
| `host/` | root, mode `0755` | session owner descriptors and the pinned `pb` the owner runs |
| `/var/lib/paperboat`, `/run/paperboat-updated` | root, mode `0755` | parents only |

`/workspace` is a second persistent volume owned by the `paperboat` runtime user. Never mount
either volume from a less-trusted container, and never copy the state volume between machines: it
holds this machine's identity.

The root filesystem is read-only in the supplied Compose definition. Only the fixed state and
workspace volumes plus tmpfs `/run` and `/tmp` are writable. The supervisor pins the runtime paths
(a deployment cannot select a socket, token or state path) and rejects unexpected ownership,
symlinks and a malformed capability token.

## Building the image

From the repository root (the cli builds next to the relay and shared modules):

```sh
docker build -f cli/deploy/container/Dockerfile --build-arg PAPERBOAT_VERSION=2026.11.17.1 -t paperboat .
```

## Self-hosted container

Use [../self-hosted/compose.yaml](../self-hosted/compose.yaml). Set `PAPERBOAT_CONTROL_URL` and, for
the first start only, `PAPERBOAT_ENROLLMENT_CREDENTIAL`; remove the credential afterwards. A restart
with the state volume needs neither a token nor a machine ID. No SSH daemon or host port is exposed;
all connectivity uses normal Paperboat direct or relay paths.

## Operational boundary

An image replacement, pod eviction, node loss or administrator container restart is
supervisor-class maintenance. Schedule it through the maintenance approval flow when protected
workloads are present, and keep the two volumes attached.
