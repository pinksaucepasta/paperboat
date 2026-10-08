## pb session shared

List owned and explicitly shared terminal sessions

### Synopsis

List owned and explicitly shared terminal sessions

List sessions you own and sessions explicitly shared with you. This inventory shows access, not a blanket right to all sessions in a team environment.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session shared [flags]
```

### Options

```
      --closed         filter explicitly by closed state
  -h, --help           help for shared
      --json           print canonical JSON
      --owner string   filter owner: mine, shared, or an authorized account ID
      --q string       filter by resource name or ID
      --state string   filter by resource state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

