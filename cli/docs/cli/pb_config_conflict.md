## pb config conflict

Inspect and resolve configuration conflicts

### Synopsis

Inspect and resolve configuration conflicts

List current path conflicts, inspect their metadata with show, and read both versions with compare through an encrypted peer connection. Resolve the exact current path by choosing the machine or repository version.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for conflict
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config
* [pb config conflict compare](pb_config_conflict_compare.md)	 - Read current machine and repository conflict contents through an encrypted connection
* [pb config conflict list](pb_config_conflict_list.md)	 - List current path conflicts
* [pb config conflict resolve](pb_config_conflict_resolve.md)	 - Choose the machine or repository version
* [pb config conflict show](pb_config_conflict_show.md)	 - Show a current path conflict

