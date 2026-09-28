## pb auth login

Sign in with a 26-character enrollment token

### Synopsis

Sign in with a 26-character enrollment token

Enter the enrollment token interactively or read it from --token-file so it is not exposed in shell history. A successful redemption stores the client session for later authenticated commands.

Sign-in credentials are stored in the selected local profile for its configured Paperboat server. Account commands use that profile; a missing or rejected session must be repaired with pb auth login before protected resources can be used.

JSON output is supported with --json.

```
pb auth login [flags]
```

### Options

```
  -h, --help                help for login
      --json                print sign-in result as JSON
      --token-file string   absolute protected file containing the enrollment token
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb auth](pb_auth.md)	 - Manage Paperboat sign-in

