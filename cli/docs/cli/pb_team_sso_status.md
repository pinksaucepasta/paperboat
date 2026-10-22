## pb team sso status

Show SSO connection, verified domains, Require SSO and exemptions

### Synopsis

Show the team's single sign-on state as last read from WorkOS: the identity provider connection and its state, verified and pending domains, whether Require SSO is on and its SSO session length, JIT provisioning, and the members marked SSO exempt. Owners, admins and auditors can read it. Use --json for every field.

JSON output is supported with --json.

```
pb team sso status <team> [flags]
```

### Options

```
  -h, --help   help for status
      --json   print JSON
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

