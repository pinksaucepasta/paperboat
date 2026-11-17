## pb team machine share

Share your personal machine with a team; grants are separate. A team that approves new machines holds it until an owner or admin accepts it

### Synopsis

Share your personal machine with the team. You keep ownership; sharing alone lets nobody
use the machine, so add a grant with pb team machine grant. If the team requires approval for
new machines (pb team machine approval), the machine waits until an owner or admin accepts it.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team machine share <team> <machine-id> [flags]
```

### Examples

```
  pb team machine share acme m_abc123 --generation 9
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for share
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

