## pb config

Inspect the local CLI config

### Synopsis

Inspect the local CLI config

With --json, this command group lists its available commands.

```
pb config [flags]
```

### Options

```
  -h, --help   help for config
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
* [pb config approve](pb_config_approve.md)	 - Approve the currently reviewed pull revision
* [pb config assign](pb_config_assign.md)	 - Assign a config repository to a machine
* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts
* [pb config customize](pb_config_customize.md)	 - Customize local shortcuts, command defaults, and TUI appearance
* [pb config force](pb_config_force.md)	 - Force a scoped configuration direction
* [pb config path](pb_config_path.md)	 - Print the config file path
* [pb config set](pb_config_set.md)	 - Set a local configuration value
* [pb config show](pb_config_show.md)	 - Print the effective config
* [pb config status](pb_config_status.md)	 - Show configuration synchronization status
* [pb config status-bar](pb_config_status-bar.md)	 - Configure the interactive terminal status bar
* [pb config team-default-adopt](pb_config_team-default-adopt.md)	 - Adopt a team default using your provider access
* [pb config team-default-set](pb_config_team-default-set.md)	 - Set a team's default pull repository
* [pb config team-default-unadopt](pb_config_team-default-unadopt.md)	 - Stop inheriting a team configuration default
* [pb config unassign](pb_config_unassign.md)	 - Remove a config repository assignment
* [pb config unset](pb_config_unset.md)	 - Remove a local configuration value

