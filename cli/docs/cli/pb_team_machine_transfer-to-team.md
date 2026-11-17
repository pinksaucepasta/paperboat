## pb team machine transfer-to-team

Explicitly transfer your enrollment to team ownership

### Synopsis

Give ownership of a machine you enrolled to the team. This changes who owns and can
revoke the machine, so --confirm must repeat the exact machine ID.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team machine transfer-to-team <team> <machine-id> [flags]
```

### Examples

```
  pb team machine transfer-to-team acme m_abc123 --confirm m_abc123 --generation 9
```

### Options

```
      --confirm string    exact machine identifier acknowledging the ownership or revocation effect
      --generation uint   expected current team generation
  -h, --help              help for transfer-to-team
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

