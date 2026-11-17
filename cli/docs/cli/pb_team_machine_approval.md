## pb team machine approval

Set whether machines members add wait for an owner or admin

### Synopsis

Choose whether machines members add to the team wait for an owner or admin. With on, a new
machine waits until pb team machine approve; with off, it joins right away.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team machine approval <team> <on|off> [flags]
```

### Examples

```
  pb team machine approval acme on
  pb team machine approval acme off
```

### Options

```
  -h, --help   help for approval
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

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

