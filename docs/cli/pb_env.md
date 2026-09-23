## pb env

Manage ENV Injection for connected hosts

### Synopsis

Manage ENV Injection for connected hosts

With --json, this command group lists its available commands.

```
pb env [flags]
```

### Options

```
  -h, --help   help for env
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb env grants](pb_env_grants.md)	 - Reconcile encrypted ENV team grants
* [pb env host](pb_env_host.md)	 - Manage encrypted host ENV projections
* [pb env list](pb_env_list.md)	 - List configured environment-variable metadata
* [pb env rotate](pb_env_rotate.md)	 - Rotate personal ENV scope keys while preserving values
* [pb env set](pb_env_set.md)	 - Set one environment variable through a hidden prompt or bounded stdin
* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes
* [pb env unset](pb_env_unset.md)	 - Remove one environment variable
* [pb env vault](pb_env_vault.md)	 - Manage password-protected ENV key custody

