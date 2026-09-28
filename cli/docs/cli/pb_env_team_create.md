## pb env team create

Create an encrypted ENV team scope

### Synopsis

Create an encrypted ENV team scope

Create an encrypted scope for the selected team. Values added later remain separate from personal ENV values and require explicit member grants.

Team ENV scopes have their own encrypted keys and explicit account grants. Rotation preserves values while replacing access keys; reset discards values. Review the confirmation and affected members before changing custody.

JSON output is supported with --json.

```
pb env team create <team> [flags]
```

### Options

```
  -h, --help   help for create
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes

