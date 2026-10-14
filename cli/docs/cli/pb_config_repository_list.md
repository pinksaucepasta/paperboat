## pb config repository list

List connected repositories

### Synopsis

List connected repositories

List the connected repositories available to your signed-in account, including their stable IDs, display names, and providers. Use the stable repository ID when names are ambiguous or when scripting later configuration commands.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config repository list [flags]
```

### Options

```
  -h, --help   help for list
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config repository](pb_config_repository.md)	 - Connect custom Git repositories and manage machine credentials

