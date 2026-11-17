## pb ssh trust-host

Approve a changed SSH host identity

### Synopsis

Approve a pending SSH host-key change for a machine. Paperboat does not accept a changed
host identity on its own: check the pending SHA256 fingerprint out of band (for example on the
machine itself) before you trust it.

With --fingerprint the command trusts only if it equals the pending fingerprint; any other
value changes nothing. Without it, the command shows the current and pending fingerprints and
asks [y/N] in a terminal, and refuses outside one. --json needs --fingerprint. It does not run
OpenSSH.

JSON output is supported with --json.

```
pb ssh trust-host <machine> [flags]
```

### Examples

```
  pb ssh trust-host homelab
  pb ssh trust-host homelab --fingerprint SHA256:abc123
  pb ssh trust-host homelab --fingerprint SHA256:abc123 --json
```

### Options

```
      --fingerprint string   exact pending SHA256 fingerprint to trust (required outside a terminal and with --json)
  -h, --help                 help for trust-host
      --json                 print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb ssh](pb_ssh.md)	 - Connect to a machine with OpenSSH

