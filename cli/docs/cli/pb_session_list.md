## pb session list

List durable terminal sessions

### Synopsis

List durable terminal sessions

List durable sessions with automatic application title or foreground process and current directory where observed. “Started in” labels the launch directory when current metadata is unavailable. --wide adds the session ID; choose an exact session ID for attach, rename, close, or sharing.

Terminal sessions persist independently of a single connection and can be reattached after a disconnect. Sharing grants explicit access to a named session. Closing, deleting, and removing a participant have different effects; inspect the selected session before acting.

JSON output is supported with --json.

```
pb session list [environment] [flags]
```

### Options

```
      --closed         filter explicitly by closed state
  -h, --help           help for list
      --json           print JSON
      --owner string   filter owner: mine, shared, or an authorized account ID
      --q string       filter by resource name or ID
      --state string   filter by resource state
      --wide           include immutable IDs
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

