## pb config assign

Assign a config repository to a machine

### Synopsis

Assign a config repository to a machine

Bind a repository to the named enrolled machine and choose pull/push behavior with --mode and repository options. Automatic updates are explicit; the confirmation code acknowledges that selected content is plaintext in private Git history.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config assign <repository> <machine> [flags]
```

### Options

```
      --automatic-updates        apply later reviewed-scope updates automatically
      --confirm string           six-character confirmation code for plaintext Git storage
  -h, --help                     help for assign
      --json                     print JSON
      --mode string              sync mode: pull-only, push-only, or bidirectional (default "pull-only")
      --pull-repository string   repository used for pulls (defaults to positional repository)
      --push-repository string   repository used for pushes (defaults to positional repository)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config](pb_config.md)	 - Inspect the local CLI config

