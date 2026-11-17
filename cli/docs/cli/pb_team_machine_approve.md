## pb team machine approve

Accept a machine a member asked to add to the team

### Synopsis

Accept a machine a member asked to add to the team (see pb team machine requests). Only
owners and admins can. --generation is optional here: without it the command uses the team's
current generation.

JSON output is supported with --json.

```
pb team machine approve <team> <machine-id> [flags]
```

### Examples

```
  pb team machine requests acme
  pb team machine approve acme m_abc123
```

### Options

```
      --generation uint   expected current team generation (default: the current one)
  -h, --help              help for approve
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

