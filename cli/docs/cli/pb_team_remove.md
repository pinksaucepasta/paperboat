## pb team remove

Remove a team member

### Synopsis

Remove a member from the team. Their team access and adopted defaults end at once.
Independently granted Git access and files or secrets they already received remain with them.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team remove <team> <account> [flags]
```

### Examples

```
  pb team remove acme ana --generation 9
```

### Options

```
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

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

