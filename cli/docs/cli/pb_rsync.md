## pb rsync

Run rsync with Paperboat machine resolution

### Synopsis

Run the system rsync with Paperboat machine names. Every argument is passed to rsync
unchanged; pb parses no flags of its own. An operand written [user@]machine:path is replaced
with the machine's managed SSH address; at least one operand must name a machine. A drive-letter
path on a Windows machine is converted to the absolute form rsync reads. What is copied or
deleted, and the exit status, are rsync's: read the command line before using --delete.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb rsync <standard rsync arguments...> [flags]
```

### Examples

```
  pb rsync -avz homelab:/srv/app/ ./app/
  pb rsync -avz ./dist/ deploy@homelab:/srv/app/
  pb rsync -avz --delete homelab:C:/data/ ./data/
```

### Options

```
  -h, --help   help for rsync
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

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

