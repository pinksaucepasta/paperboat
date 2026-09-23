## pb team machine remove

Revoke a team-owned machine without personal takeover

### Synopsis

Revoke a team-owned machine without personal takeover

JSON output is supported with --json.

```
pb team machine remove <team> <machine-id> [flags]
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
```

### SEE ALSO

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

