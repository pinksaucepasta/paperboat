## pb team machine remove

Revoke a team-owned machine without personal takeover

### Synopsis

Revoke a team-owned machine's enrollment without handing it to a person. --confirm must
repeat the exact machine ID. If the device was lost or compromised, rotate the team's ENV keys
afterwards.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team machine remove <team> <machine-id> [flags]
```

### Examples

```
  pb team machine remove acme m_abc123 --confirm m_abc123 --generation 9
```

### Options

```
      --confirm string    exact machine identifier acknowledging the ownership or revocation effect
      --generation uint   expected current team generation
  -h, --help              help for remove
      --json              print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

