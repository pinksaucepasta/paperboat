## pb sftp

Run sftp with Paperboat machine resolution

### Synopsis

Run the system sftp with Paperboat machine names. Every argument is passed to sftp
unchanged; pb parses no flags of its own. The destination is the first argument that is not an
option (after -- if present), written [user@]machine with an optional :path, and it is replaced
with the machine's managed SSH address. Host identity checks stay on.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb sftp <standard sftp arguments...> [flags]
```

### Examples

```
  pb sftp homelab
  pb sftp deploy@homelab:/srv/app
  pb sftp -P 2222 homelab
```

### Options

```
  -h, --help   help for sftp
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

