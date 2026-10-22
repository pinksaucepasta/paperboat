## pb auth login

Add this computer to your account, or sign in again, through browser approval

### Synopsis

Add this computer to your account, or sign in again, through browser approval

Print an approval link and open it when a browser is available. Sign in through WorkOS on this or another device, choose the workspace and explicitly approve; a computer that is not on your account yet is added to that workspace. No local callback listener or copied token is needed. Interrupted attempts resume securely; the existing account remains active until replacement succeeds.

Sign-in credentials are stored in the selected local profile for its configured Paperboat server. Account commands use that profile; a missing or rejected session must be repaired with pb auth login before protected resources can be used.

JSON output is supported with --json.

```
pb auth login [flags]
```

### Options

```
      --api-token-file string   sign in with an API token read from this file (- for stdin) instead of browser approval
      --change-account          sign in with another account after browser approval
      --fresh                   remove this computer's setup and machine record, then add it again
  -h, --help                    help for login
      --json                    print approval and sign-in states as JSON
      --no-browser              print the approval link without opening a browser
      --reauth                  authenticate the current account again
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

