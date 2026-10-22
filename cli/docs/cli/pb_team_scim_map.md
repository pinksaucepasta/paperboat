## pb team scim map

Map a directory group to a team role

### Synopsis

Map a directory group, by name or ID, to a team role: admin, billing, auditor or member, or none to remove the mapping. Someone in several mapped groups gets the highest role. With --mapped-only, only members of mapped groups are provisioned and everyone else is removed. Members managed by the directory change only through it.

JSON output is supported with --json.

```
pb team scim map <team> <group> <admin|billing|auditor|member|none> [flags]
```

### Options

```
  -h, --help          help for map
      --json          print JSON
      --mapped-only   provision only members of mapped groups
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

