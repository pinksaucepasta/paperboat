## pb env team grant

Grant an account access to an encrypted ENV team scope

### Synopsis

Grant an account access to an encrypted ENV team scope

Give one account access to the selected encrypted team ENV scope. The account must still accept and reconcile its grant before using values.

Team ENV scopes have their own encrypted keys and explicit account grants. Rotation preserves values while replacing access keys; reset discards values. Review the confirmation and affected members before changing custody.

JSON output is supported with --json.

```
pb env team grant <team> <account> [flags]
```

### Options

```
  -h, --help   help for grant
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

