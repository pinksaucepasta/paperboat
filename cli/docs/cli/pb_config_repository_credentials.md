## pb config repository credentials

Store credentials privately on this machine

### Synopsis

Store credentials privately on this machine

Set or remove a private authentication profile for a custom Git repository on this machine. Profiles remain local to the machine and are configured independently from repository registration and the paths selected for synchronization.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for credentials
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

* [pb config repository](pb_config_repository.md)	 - Connect custom Git repositories and manage machine credentials
* [pb config repository credentials remove](pb_config_repository_credentials_remove.md)	 - Remove this machine's saved repository credentials
* [pb config repository credentials set](pb_config_repository_credentials_set.md)	 - Read a credential profile from a private JSON file or stdin

