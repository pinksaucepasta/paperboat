## pb config sync schema

Print the config sync file JSON Schema

### Synopsis

Print the JSON Schema of config-sync.jsonc, the shared file in a repository and a
machine's own file, for editors and AI agents that write it. The schema is built into pb,
so this command works offline and needs no sign-in. Point a file at it with a "$schema" key.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb config sync schema [flags]
```

### Examples

```
  pb config sync schema > config-sync.schema.json
```

### Options

```
  -h, --help   help for schema
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

* [pb config sync](pb_config_sync.md)	 - Validate and apply this machine's configuration file

