## pb inbox path

Show this machine's Inbox directory

### Synopsis

Print the directory on this machine where received files are written. It is a setting of
this machine only and has nothing to do with where your own sends go.

JSON output is supported with --json.

```
pb inbox path [flags]
```

### Examples

```
  pb inbox path
  pb inbox path --json
```

### Options

```
  -h, --help   help for path
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

