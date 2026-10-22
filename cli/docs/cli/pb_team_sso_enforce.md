## pb team sso enforce

Turn Require SSO on or off (shows who loses access first)

### Synopsis

Turn Require SSO on or off. When on, members reach the team only after an SSO sign-in within the SSO session length, set with --session. Turning it on first lists the members who would lose access until they sign in through SSO and prints the exact command with --confirm to proceed. Open connections keep running.

JSON output is supported with --json.

```
pb team sso enforce <team> <on|off> [flags]
```

### Options

```
      --confirm string   the review code printed when turning Require SSO on
  -h, --help             help for enforce
      --json             print JSON
      --session string   SSO session length, e.g. 24h or 7d (1–30 days)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team sso](pb_team_sso.md)	 - Single sign-on for a team (owners and admins)

