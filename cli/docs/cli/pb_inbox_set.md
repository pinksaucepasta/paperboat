## pb inbox set

Set this machine's Inbox directory

### Synopsis

Set the directory received files are written to. Give a full path on this computer, written
the way this computer writes paths; ~ at the start means your home directory. A relative path is
refused. The directory is created if it does not exist and must be writable only by you.
Files already delivered stay where they are.

JSON output is supported with --json.

```
pb inbox set <directory> [flags]
```

### Examples

```
  pb inbox set ~/Inbox
  pb inbox set /srv/incoming --json
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

