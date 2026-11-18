## pb recordings key reset

Replace a recording key nobody holds any more; older recordings become unreadable

### Synopsis

Reset the team's recording key when nobody holds it any more.

Only the team owner can. The new key is made here and signed with your ENV vault's writer key, the identity the ENV team-vault reset uses; machines accept it only because that signature verifies under the key they pinned for the team, never because the server says so. Everyone who can replay gets a copy of the new key. Recordings made before cannot be opened by anyone, you included: use `pb recordings key replace` instead while any member still holds the key.

Needs an unlocked ENV vault (`pb env unlock`).

JSON output is supported with --json.

```
pb recordings key reset [flags]
```

### Options

```
      --confirm string   the team id, typed to confirm
  -h, --help             help for reset
      --json             print JSON
      --team string      team whose key to reset; defaults to the active workspace
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb recordings key](pb_recordings_key.md)	 - Manage a team's recording key

