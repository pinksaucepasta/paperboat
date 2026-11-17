## pb team machine decline

Decline a machine a member asked to add to the team

### Synopsis

Decline a machine a member asked to add to the team. Only owners and admins can.
--generation is optional: without it the command uses the team's current generation.

JSON output is supported with --json.

```
pb team machine decline <team> <machine-id> [flags]
```

### Examples

```
  pb team machine decline acme m_abc123
```

### Options

```
      --generation uint   expected current team generation (default: the current one)
  -h, --help              help for decline
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

