## pb mcp

Serve Paperboat tools over MCP stdio

### Synopsis

Serve list_machines, exec and send_file using your existing Paperboat login.
For automation, first sign in with pb login --api-token-file.
Exec uses argv without a shell, bounded output and a maximum five-minute deadline.
send_file requires --allow-send-dir and delivers to the receiving machine's Inbox.
This uses existing CLI access; the local Paperboat daemon and enrolled source are still required.
Standard input/output are reserved for MCP; configure your MCP client to launch pb mcp.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb mcp [flags]
```

### Options

```
      --allow-send-dir stringArray   allow sending files within this local directory (repeatable; symlinks cannot escape)
  -h, --help                         help for mcp
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

