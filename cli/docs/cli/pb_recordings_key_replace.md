## pb recordings key replace

Replace the team's recording key with a new one signed by your copy of the current key

### Synopsis

Replace the team's recording key.

The new key is made and signed here with your ENV vault: only a member who holds a copy of the current key can sign the next one, so machines follow it through the signed chain and never because the server says so. Everyone who can replay gets a copy of the new key, and older recordings stay readable through a link sealed to it. The key and its private halves never leave this computer in plaintext.

Needs the security permission on the team and an unlocked ENV vault (`pb env unlock`).

JSON output is supported with --json.

```
pb recordings key replace [flags]
```

### Options

```
      --confirm REPLACE RECORDING KEY <team>   must be REPLACE RECORDING KEY <team>
  -h, --help                                   help for replace
      --json                                   print JSON
      --team string                            team whose key to replace; defaults to the active workspace
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

