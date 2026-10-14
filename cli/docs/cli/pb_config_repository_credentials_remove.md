## pb config repository credentials remove

Remove this machine's saved repository credentials

### Synopsis

Remove this machine's saved repository credentials

Resolve a custom Git repository by ID or name and remove this machine's saved authentication profile. The repository registration and its content remain available; configure another profile before operations that require private repository authentication.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config repository credentials remove <repository> [flags]
```

### Options

```
  -h, --help   help for remove
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

* [pb config repository credentials](pb_config_repository_credentials.md)	 - Store credentials privately on this machine

