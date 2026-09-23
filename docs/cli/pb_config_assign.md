## pb config assign

Assign a config repository to a machine

### Synopsis

Assign a config repository to a machine

JSON output is supported with --json.

```
pb config assign <repository> <machine> [flags]
```

### Options

```
      --automatic-updates        apply later reviewed-scope updates automatically
  -h, --help                     help for assign
      --json                     print JSON
      --mode string              sync mode: pull-only, push-only, or bidirectional (default "pull-only")
      --pull-repository string   repository used for pulls (defaults to positional repository)
      --push-repository string   repository used for pushes (defaults to positional repository)
      --yes                      acknowledge plaintext private-Git storage and history
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config

