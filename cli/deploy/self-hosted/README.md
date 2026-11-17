# Run a Paperboat machine in a container

`compose.yaml` runs one Paperboat machine as a container, for a server that should be reachable through
Paperboat but where you do not want to install `pb` on the host. It is a *machine*: you reach it with
`pb connect`, `pb ssh`, `pb exec`, previews and file transfers like any other. To host the relay or the
tunnel edge themselves, use [pbh](../../../helper/README.md) instead.

How the image is built and what it contains is in [../container/README.md](../container/README.md).

## What you need

| Variable | Meaning |
|---|---|
| `PAPERBOAT_IMAGE` | A signed Paperboat release image. Required. |
| `PAPERBOAT_RELEASE_REPOSITORY` | The HTTPS address of the TUF release repository that signs updates. Required; the container refuses anything that is not `https://`. |
| `PAPERBOAT_CONTROL_URL` | The HTTPS address of the Paperboat control plane. Required. |
| `PAPERBOAT_MACHINE_ID` | The ID of the machine record this container becomes. Required. |
| `PAPERBOAT_ENROLLMENT_CREDENTIAL` | The one-time enrollment credential for that machine. Needed on the first start only. |

Put them in an `.env` file next to `compose.yaml` or export them. Compose stops with the variable's name if
a required one is missing.

## First start

```sh
docker compose -f compose.yaml up -d
docker compose -f compose.yaml logs -f
```

The first start consumes the one-time enrollment credential and enrolls the machine. When `pb machine list`
on another computer shows it online, **remove `PAPERBOAT_ENROLLMENT_CREDENTIAL` from your `.env` or
deployment configuration**, then recreate the container (`docker compose up -d`). The credential is
single-use and no longer needed; leaving it in a file only leaves a secret lying around.

If the container exits at once, its last line starts with `paperboat container:` and names the missing or
unsafe item (an unset variable, a non-HTTPS repository, a volume with the wrong owner).

## What it exposes and keeps

- **No host ports and no SSH daemon.** All connectivity goes out through normal Paperboat direct or relay
  paths. Nothing needs to be opened on the host's firewall.
- **Two volumes.** `paperboat-state` (`/var/lib/paperboat`) holds the installation: releases, the updater's
  journal, the machine's identity and runtime state. `paperboat-workspace` (`/workspace`) is the working
  directory and the container user's home. Keep both attached. Never mount either from a less-trusted
  container, and never copy the state volume to another host: it holds this installation's identity.
- **A read-only root filesystem**, with `/run` and `/tmp` as memory-backed tmpfs.

## Updates

Runtime and CLI updates install from the signed repository without restarting the container or ending
your terminals. Replacing the image, restarting the container or moving it to another node is different:
it ends running work, so do it when nothing you care about is open. Keep the volumes attached across it
and the machine comes back as the same machine.

## Remove it

```sh
docker compose -f compose.yaml down        # stops it; the volumes and the machine remain
docker compose -f compose.yaml down -v     # also deletes both volumes, including the identity
pb machine revoke <machine>                # from another computer: removes the machine from your account
```

Deleting the volumes without revoking leaves an offline machine in your account.
