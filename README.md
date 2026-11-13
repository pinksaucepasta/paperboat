# Paperboat

Access to your own machines from anywhere: terminals that survive disconnects, remote
exec, SSH/SCP/rsync, file transfer, preview links and tunnels, and encrypted environment
variables. Connections are end-to-end encrypted and peer-to-peer where the network allows.
Paperboat runs in userspace, so it works alongside your existing VPN instead of replacing it.

Paperboat is in beta. Linux and macOS (Apple Silicon) are supported; Windows is in progress.

## Using

Install with the command shown in the web console, then:

```sh
pb login              # sign in and add this computer
pb machine list       # list your machines
pb <machine>          # open a terminal on one
pb preview 3000       # share a local port as a preview link
```

The [CLI guide](cli/docs/cli.md) and [command reference](cli/docs/cli/README.md) cover the rest.

## Repository

| Directory | What |
|---|---|
| `cli/` | `pb`, the CLI and the background service that runs on each machine |
| `server/` | control plane: accounts, teams, authorization |
| `relay/` | relay used when a direct connection is not possible |
| `tunnel/` | edge for preview links and public tunnels |
| `helper/` | `pbh`, installs and manages a self-hosted relay and edge |
| `shared/` | Go packages used by more than one of the above |
| `console/` | web console (Next.js) |
| `marketing/` | website (Astro) |

## Building

Go 1.27 or later. The Go modules are tied together by `go.work`.

```sh
make build    # binaries in .build/
make test     # vet and unit tests
```

## Bugs and security

Report bugs in [issues](https://github.com/pinksaucepasta/paperboat/issues). Report security problems privately, see [SECURITY.md](SECURITY.md).

## License

`server/` and `console/` are AGPL-3.0; everything else is MIT. See [LICENSING.md](LICENSING.md).
