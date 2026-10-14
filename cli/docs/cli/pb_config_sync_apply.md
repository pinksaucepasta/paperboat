## pb config sync apply

Review the effective file configuration on this machine

### Synopsis

Review the effective file configuration on this machine

Read and resolve the machine configuration, preview its effective assignment, then apply the confirmed revision. Use --confirm with the displayed code in noninteractive use. Synced files are plaintext in Git, and prior content remains in repository history.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config sync apply [flags]
```

### Options

```
      --confirm string      six-character confirmation code from the preview
  -h, --help                help for apply
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

