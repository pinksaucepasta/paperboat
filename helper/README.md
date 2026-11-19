# pbh

`pbh` installs and manages a self-hosted Paperboat relay and/or tunnel edge on one Linux host. The
relay carries your machines' connections when they cannot reach each other directly; the edge serves
your previews and tunnels. Installing a node assigns it to no account: you claim it afterwards in the
dashboard (Network → Add self-hosted node).

Requirements: Ubuntu 22.04/24.04 or Debian 12 on amd64 or arm64, systemd, Docker (the installer adds it
from Docker's official apt repository when missing), and public inbound connectivity.

## Install

```sh
curl -fsSL https://get.pprbt.dev/selfhost | sh                              # asks what to run
curl -fsSL https://get.pprbt.dev/selfhost | sh -s -- --relay                  # or --edge, or --both: no questions
```

It prints a claim address and a single-use code that lasts fifteen minutes. Paste both into the dashboard.
`pbh selfhost code` prints a fresh one. Running the same command again upgrades in place and keeps your data.
Ports, certificates, DNS and flood protection are described in [deploy/README.md](deploy/README.md).

## Commands

Run `pbh help` for the list and `pbh <command> --help` for flags and examples.

| Command | What it does |
|---|---|
| `pbh install` | Install or upgrade the node and its systemd service (`paperboat-selfhost.service`). |
| `pbh selfhost code` | Print the claim address and a fresh single-use code. |
| `pbh status` | Show the service state. |
| `pbh logs [--follow] [relay\|edge]` | Show the relay and edge container logs, bounded by `--lines` and `--since`. |
| `pbh restart [relay\|edge]` | Restart the service, or only the relay or the edge. |
| `pbh update` | Update to the latest signed release (the dashboard's Update button runs the same steps); shows current → latest, asks to confirm, rolls back on failure. |
| `pbh doctor` | Check that the host can serve public tunnel ports (firewall, connection limits, reachability from the control plane). |
| `pbh configure-tls` | Set or rotate the browser-trusted HTTPS certificate for a tunnel node. |
| `pbh uninstall` | Remove the service and containers, keeping identity and configuration unless you confirm deleting them. |
| `pbh serve` | Run the node in the foreground; the systemd unit runs this. |
| `pbh check-host`, `pbh version` | Check that the host is supported; print the version. |

## Layout

| Path | What |
|---|---|
| `cmd/pbh/` | the command |
| `internal/containers/` | the Docker lifecycle of the relay and edge containers |
| `deploy/` | the installer, its packaging and publication scripts, and the full operator guide |

The relay and edge programs themselves live in [`../relay`](../relay) and [`../edge`](../edge); `pbh`
runs their container images and writes their configuration.

## Development

From the repository root, `go test ./helper/...` and `go vet ./helper/...`.

## License

MIT. See [LICENSE](LICENSE).
