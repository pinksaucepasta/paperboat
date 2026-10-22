## pb team sso exempt

Mark or unmark a member as SSO exempt (break-glass)

### Synopsis

Mark a member as SSO exempt so they keep access to the team without SSO, for contractors and emergency admins, or remove the mark with --remove. The owner is always exempt. Only a few members may be exempt, and every access without SSO is recorded in the audit log once a day per person.

JSON output is supported with --json.

```
pb team sso exempt <team> <account> [flags]
```

### Options

```
  -h, --help     help for exempt
      --json     print JSON
      --remove   remove the exemption
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

