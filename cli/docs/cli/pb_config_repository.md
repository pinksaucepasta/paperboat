## pb config repository

Connect custom Git repositories and manage machine credentials

### Synopsis

Connect custom Git repositories and manage machine credentials

Connect a Git repository to your account, inspect registered repositories, and configure authentication separately on this machine. Connecting a repository does not select local files; define explicit machine path rules before enabling synchronization.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for repository
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
* [pb config repository connect](pb_config_repository_connect.md)	 - Connect an HTTPS, SSH, or local Git repository
* [pb config repository credentials](pb_config_repository_credentials.md)	 - Store credentials privately on this machine
* [pb config repository list](pb_config_repository_list.md)	 - List connected repositories

