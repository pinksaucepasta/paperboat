## pb team scim

Directory sync (SCIM) for a team (owners and admins)

### Synopsis

Directory sync for a team through WorkOS: connect your company directory (Okta, Entra ID, Google Workspace and others) and Paperboat adds, updates and removes members to match it within seconds. Map directory groups to team roles. Removing someone in the directory removes them from the team at once and ends their open sessions.

With --json, this command group lists its available commands.

```
pb team scim [flags]
```

### Options

```
  -h, --help   help for scim
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
* [pb team scim map](pb_team_scim_map.md)	 - Map a directory group to a team role
* [pb team scim setup](pb_team_scim_setup.md)	 - Open the WorkOS Admin Portal to connect your directory
* [pb team scim status](pb_team_scim_status.md)	 - Show directory sync state, last sync and group roles

