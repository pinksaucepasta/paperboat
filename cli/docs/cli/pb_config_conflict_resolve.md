## pb config conflict resolve

Choose the machine or repository version

### Synopsis

Choose the machine or repository version

Resolve one exact path with --keep to select the machine or repository version. Use compare first to inspect both sides; the chosen direction is recorded for the next synchronization step.

Conflicts identify exact paths whose local and repository versions cannot be merged automatically. Review the affected path before choosing a side; resolution records a deliberate decision for the next synchronization step.

JSON output is supported with --json.

```
pb config conflict resolve <environment> <path> [flags]
```

### Options

```
  -h, --help          help for resolve
      --json          print JSON
      --keep string   version to keep: machine or repository
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts

