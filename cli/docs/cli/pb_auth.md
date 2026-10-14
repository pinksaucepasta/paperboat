## pb auth

Manage Paperboat sign-in

### Synopsis

Manage Paperboat sign-in

Use login for browser approval on any device, switch for Personal/team workspace selection, status to inspect the active profile, and logout to revoke it. Installation tokens remain part of machine setup, not interactive login.

With --json, this command group lists its available commands.

```
pb auth [flags]
```

### Options

```
  -h, --help   help for auth
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

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb auth login](pb_auth_login.md)	 - Sign in through browser approval on any device
* [pb auth logout](pb_auth_logout.md)	 - Revoke and remove the active client session
* [pb auth status](pb_auth_status.md)	 - Show the active Paperboat account
* [pb auth switch](pb_auth_switch.md)	 - Switch the active Personal or team workspace

