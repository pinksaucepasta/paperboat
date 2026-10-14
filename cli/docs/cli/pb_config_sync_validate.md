## pb config sync validate

Review the effective file configuration on this machine

### Synopsis

Review the effective file configuration on this machine

Read this machine's configuration, resolve its repositories and explicit mapped paths, and preview the safe effective synchronization projection. Use --repository for initial repository selection. Validation does not apply a new machine assignment or start synchronization.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config sync validate [flags]
```

### Options

```
  -h, --help                help for validate
      --json                print safe effective projection as JSON
      --repository string   registered repository ID or name for initial bootstrap
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config sync](pb_config_sync.md)	 - Validate and apply this machine's configuration file

