## pb team scim status

Show directory sync state, last sync and group roles

### Synopsis

Show the team's directory sync state: the connected directory and its state, the number of directory users, when it last synced and any problem, whether only mapped groups are provisioned, and every directory group with the team role it maps to. Owners, admins and auditors can read it. Use --json for every field.

JSON output is supported with --json.

```
pb team scim status <team> [flags]
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

* [pb team scim](pb_team_scim.md)	 - Directory sync (SCIM) for a team (owners and admins)

