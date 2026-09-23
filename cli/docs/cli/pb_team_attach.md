## pb team attach

Attach or detach an explicitly selected personal resource

### Synopsis

Attach or detach an explicitly selected personal resource

JSON output is supported with --json.

```
pb team attach <team> <preview|tunnel> <resource> [flags]
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
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

