# Managed ssh config path form on Windows, 2026-11-12 (winpool runner, Windows Server 2025)
Both ssh flavours read the same ~/.ssh config: built-in OpenSSH_for_Windows_10.0p2 and Git for Windows MSYS OpenSSH_10.6p1.
Checked: the option's executable is accepted as absolute, stat'ed, and run (KnownHostsCommand: hostname.exe, exit 0, its output read; ProxyCommand: pb.exe __ssh-proxy started).

| form of `...\Paperboat\...\pb.exe` | built-in OpenSSH | Git for Windows ssh |
|---|---|---|
| `C:\Program Files\...` (the old output) | KnownHostsCommand ok, ProxyCommand ok | KnownHostsCommand "path is not absolute"; ProxyCommand runs through cmd.exe and mis-parses ("'SSH-2.0-OpenSSH_10.6' is not recognized") |
| `C:/Program Files/...` | ok | KnownHostsCommand "path is not absolute"; ProxyCommand mis-parsed |
| `/c/Program Files/...` | "Could not stat" | ok |
| `//?/C:/Program Files/...` | ok | ok (KnownHostsCommand ran: "truncated line at KnownHostsCommand-ORDER" is its output being parsed; ProxyCommand ran) |

`//?/C:/` is the one form both accept, for ProxyCommand and KnownHostsCommand. The config writer now emits it (windowsOpenSSHExecutable).
Not checked: a full managed alias login through Git ssh (the shared test account is at its managed-ssh key capacity, so the runner got no managed config); the generated config text is built from the same executable string.
