## pb send destination clear

Clear the default transfer destination

### Synopsis

Clear the saved default send destination, account-wide or for one --session. Transfers
already made and files already received are unaffected; later sends need --to again.

JSON output is supported with --json.

```
pb send destination clear [flags]
```

### Examples

```
  pb send destination clear
```

### Options

```
  -h, --help   help for clear
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

