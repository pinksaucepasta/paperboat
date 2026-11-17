## pb send destination set

Set the default transfer destination

### Synopsis

Save a machine, by name or ID, as the default destination for pb send: account-wide, or for
one terminal session with --session. The machine is looked up first, so a wrong name is refused.

JSON output is supported with --json.

```
pb send destination set <machine> [flags]
```

### Examples

```
  pb send destination set laptop
  pb send destination set laptop --session S123
```

### Options

```
  -h, --help   help for set
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --session string     terminal session ID for a session-specific destination
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb send destination](pb_send_destination.md)	 - Show the default transfer destination

