## pb auth logout

Revoke and remove the active client session

### Synopsis

Revoke and remove the active client session

Revoke the current client session at the server and remove its local credential. If server cancellation cannot complete, the command reports the pending state so revocation can be retried.

Sign-in credentials are stored in the selected local profile for its configured Paperboat server. Account commands use that profile; a missing or rejected session must be repaired with pb auth login before protected resources can be used.

JSON output is supported with --json.

```
pb auth logout [flags]
```

### Options

```
  -h, --help   help for logout
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

