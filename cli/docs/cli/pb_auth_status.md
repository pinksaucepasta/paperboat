## pb auth status

Show the active Paperboat account

### Synopsis

Show the active Paperboat account

Report the active account and session for the configured server. Use this before an account-scoped operation to check which identity the CLI will use.

Sign-in credentials are stored in the selected local profile for its configured Paperboat server. Account commands use that profile; a missing or rejected session must be repaired with pb auth login before protected resources can be used.

JSON output is supported with --json.

```
pb auth status [flags]
```

### Options

```
  -h, --help   help for status
      --json   
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb auth](pb_auth.md)	 - Manage Paperboat sign-in

