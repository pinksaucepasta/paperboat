## pb team scim setup

Open the WorkOS Admin Portal to connect your directory

### Synopsis

Print, and open in a browser where possible, a short-lived WorkOS Admin Portal link to connect your company directory with SCIM. Your IT admin follows the guide for your directory there. Once connected, Paperboat provisions members within seconds; map groups to roles with pb team scim map. The team needs enough seats.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team scim setup <team> [flags]
```

### Options

```
  -h, --help   help for setup
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

* [pb team scim](pb_team_scim.md)	 - Directory sync (SCIM) for a team (owners and admins)

