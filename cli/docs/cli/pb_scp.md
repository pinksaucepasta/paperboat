## pb scp

Run scp with Paperboat machine resolution

### Synopsis

Run the system scp with Paperboat machine names. Every argument is passed to scp unchanged:
pb parses no flags of its own. An operand written [user@]machine:path is replaced with the
machine's managed SSH address; other operands (local paths, URLs, Windows drive paths) are left
alone. At least one operand must name a machine. Host identity checks stay on, and you must be
signed in with the machine paired.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb scp <standard scp arguments...> [flags]
```

### Examples

```
  pb scp homelab:/var/log/app.log .
  pb scp ./bundle.tar.gz deploy@homelab:/srv/app/
  pb scp -r homelab:/srv/data ./backup/
```

### Options

```
  -h, --help   help for scp
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

