## pb env team

Manage encrypted ENV team scopes

### Synopsis

Manage encrypted ENV team scopes

Create team ENV scopes, grant access, rotate keys, revoke members, or reset values. These operations affect encrypted team custody rather than ordinary team membership alone.

ENV values are encrypted before they leave the client. Commands select personal, team, or host scope explicitly; list output shows metadata rather than secret values. Unlock the local vault when a key operation requires it.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for team
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

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts
* [pb env team create](pb_env_team_create.md)	 - Create an encrypted ENV team scope
* [pb env team grant](pb_env_team_grant.md)	 - Grant an account access to an encrypted ENV team scope
* [pb env team reset](pb_env_team_reset.md)	 - Discard all values and replace an encrypted ENV team key
* [pb env team revoke](pb_env_team_revoke.md)	 - Rotate an ENV team scope and revoke selected members
* [pb env team rotate](pb_env_team_rotate.md)	 - Rotate an encrypted ENV team key while preserving its values

