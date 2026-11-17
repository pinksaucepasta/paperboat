## pb inbox reset

Restore the default Inbox directory

### Synopsis

Restore the default receiving directory, ~/Paperboat/Inbox, for future files. Files already
delivered stay where they are, and the account's team request policy is unchanged. Use
pb inbox set to choose another directory.

JSON output is supported with --json.

```
pb inbox reset [flags]
```

### Examples

```
  pb inbox reset
```

### Options

```
  -h, --help   help for reset
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

