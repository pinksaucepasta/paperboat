## pb team sso

Single sign-on for a team (owners and admins)

### Synopsis

Single sign-on for a team through WorkOS, with SAML or OpenID Connect and any identity provider such as Okta, Entra ID or Google Workspace. Verify your domain, connect the identity provider in the WorkOS Admin Portal, then optionally require SSO so members reach the team only after signing in through it. The owner and SSO-exempt members are the break-glass accounts.

With --json, this command group lists its available commands.

```
pb team sso [flags]
```

### Options

```
  -h, --help   help for sso
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

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions
* [pb team sso enforce](pb_team_sso_enforce.md)	 - Turn Require SSO on or off (shows who loses access first)
* [pb team sso exempt](pb_team_sso_exempt.md)	 - Mark or unmark a member as SSO exempt (break-glass)
* [pb team sso setup](pb_team_sso_setup.md)	 - Open the WorkOS Admin Portal to verify a domain and connect your identity provider
* [pb team sso status](pb_team_sso_status.md)	 - Show SSO connection, verified domains, Require SSO and exemptions

