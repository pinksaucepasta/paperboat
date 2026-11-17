## pb team leave

Leave a team

### Synopsis

Leave a team yourself. An owner must transfer ownership first (pb team transfer) before
leaving.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team leave <team> [flags]
```

### Examples

```
  pb team leave acme --generation 9
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for leave
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

