## pb team attach

Attach or detach an explicitly selected personal resource

### Synopsis

Attach one of your personal previews or tunnels to the team so team grants can refer to it,
or detach it with --active=false. Only previews and tunnels can be attached.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team attach <team> <preview|tunnel> <resource> [flags]
```

### Examples

```
  pb team attach acme preview blog --generation 9
  pb team attach acme preview blog --active=false --generation 10
```

### Options

```
      --active            attach resource; set false to detach (default true)
      --generation uint   expected current team generation
  -h, --help              help for attach
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

