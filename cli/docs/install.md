# Install Paperboat

`pb` is one static executable. It runs on Linux (amd64, arm64), macOS (Apple Silicon) and
Windows (amd64, arm64). Install it on every computer you want to reach and on the ones you reach
them from; there is no separate client and server.

## Install and sign in

Linux and macOS:

```sh
curl -fsSL https://get.pprbt.dev | sh
```

Windows (PowerShell):

```powershell
irm https://get.pprbt.dev | iex
```

The installer puts `pb` on the machine, starts its background service, and signs the machine in. On a
computer with a browser it opens an approval page. On a server with no browser it prints a link and a short
code; open the link on any device and approve. On Windows the installer asks for administrator approval to register the service.

Check the result:

```sh
pb --version
pb auth status
pb doctor
```

## Add another machine without approving each one

```sh
pb machine add --name studio
```

This prints an install command for Linux/macOS and one for Windows (PowerShell). Both carry the same one-time token
in the environment (`PB_TOKEN`), never in the address. Run the one for the new machine within ten minutes (the default lifetime; `expires_at` is in
`pb machine add --json`). The token enrolls that one machine, then stops working.

## What the installer checks

The installer is one fixed script in the Paperboat repository (`cli/tools/install.sh`, `install.ps1`); no Paperboat
server takes part in the first install. It resolves the latest GitHub release, downloads `pb` with that release's
`SHA256SUMS` and `SHA256SUMS.sig`, and verifies the signature with the release key embedded in the script before it
trusts any hash. Then it checks the download against `SHA256SUMS` and refuses on any mismatch. `ssh-keygen` does the
verification; when it is missing the installer installs OpenSSH with your package manager (or the Windows OpenSSH
client) and never skips the check. Set `PB_VERSION` to install one release instead of the latest, and `PB_RECIPE` to
pair from a recipe. Once installed, `pb` verifies every later update against signed (TUF) release metadata, with
rollback protection. The technical contract is in [tuf-release.md](tuf-release.md).

The Windows executables are not Authenticode-signed; their trust comes from the release signature.
Windows SmartScreen may warn if you download `pb.exe` by hand in a browser. The PowerShell command above
does not trigger it.

## Where things go

| | Linux | macOS | Windows |
|---|---|---|---|
| `pb` command | `~/.local/bin/pb` (choose another absolute directory with `pb install --install-dir`) | chosen by the installer, printed by `pb install --json` as `data.executable` | chosen by the installer, same output |
| Background service | `paperboatd.service`, a systemd user service | `com.pinksaucepasta.paperboatd`, a launchd agent in your GUI session | a Windows service registered with administrator approval |
| Manual pages | installed with `pb` (`man pb`, `man pb-tunnel-create`) | same | — |

The service starts on demand: `pb status` and `pb wait` install and start it if it is missing and wait up
to 30 seconds. Details are in [operations.md](operations.md).

## Install a build you already have

```sh
./pb install
```

`pb install` installs the executable you run, without contacting a release server, then selects the
per-user location above. `--install-dir DIR` is Unix-only and is not supported on macOS. A custom (source)
build can be installed this way but cannot check for or install updates.

## Updates

Official installations update themselves: a release is downloaded ahead of 04:00 on the machine's clock
and installed then, keeping your terminals and sessions. `pb update` updates now, `pb update status`
shows where it stands, and `pb update settings --auto=false` keeps the availability check but leaves
installing to you. Running the install command again upgrades in place and keeps your enrollment and
settings.

## Repair

Run `pb doctor`. On a paired Windows computer `pb doctor --repair` also re-registers the Windows
service (administrator approval required); on Linux and macOS it changes nothing, and the service is
reinstalled the next time a command needs it. If a machine says it is signed out, `pb login` signs it in
again.

## Uninstall

```sh
pb uninstall
```

It asks you to type `UNINSTALL PAPERBOAT` and the machine's hostname, then removes the service, the
binaries, the credentials, the configuration and the runtime state. Your Paperboat Inbox and the files in
it are kept. For scripts, pass `--confirmation "UNINSTALL PAPERBOAT" --hostname <hostname> --json`.
The machine stays in your account until you remove it with `pb machine revoke <machine>` or in the
dashboard.

## Running Paperboat in a container

See [../deploy/self-hosted/README.md](../deploy/self-hosted/README.md).
