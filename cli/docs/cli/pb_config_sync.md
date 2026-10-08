## pb config sync

Validate and apply this machine's configuration file

### Synopsis

Validate and apply this machine's configuration file

Inspect, initialize, validate, or apply this machine's authoritative configuration file. The file defines repository choices and explicit path rules. Validate the effective configuration before applying a confirmed change to the enrolled machine's synchronization assignment.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for sync
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
* [pb config sync apply](pb_config_sync_apply.md)	 - Review the effective file configuration on this machine
* [pb config sync init](pb_config_sync_init.md)	 - Create an empty machine configuration without overwriting existing files
* [pb config sync path](pb_config_sync_path.md)	 - Show the authoritative machine configuration path
* [pb config sync validate](pb_config_sync_validate.md)	 - Review the effective file configuration on this machine

