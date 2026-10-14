## pb auth switch

Switch the active Personal or team workspace

### Synopsis

Choose an available workspace for resource discovery and new operations. Running operations and this machine's enrollment remain unchanged. Use Personal for personally owned resources or a team slug for team resources; switching never changes the machine selected for enrollment. The selected workspace is saved for this account and server. --workspace overrides PAPERBOAT_WORKSPACE, which overrides the saved default; without either, Personal is active.

Alias for pb switch. Select Personal or an available team for resource discovery and new operations. The selection is scoped to the signed-in account and server; running terminals, previews and tunnels keep their existing ownership.

Sign-in credentials are stored in the selected local profile for its configured Paperboat server. Account commands use that profile; a missing or rejected session must be repaired with pb auth login before protected resources can be used.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb auth switch [workspace] [flags]
```

### Options

```
  -h, --help   help for switch
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

* [pb auth](pb_auth.md)	 - Manage Paperboat sign-in

