## pb machine revoke

Disconnect and revoke a machine

### Synopsis

Disconnect a machine and revoke its enrollment: its credentials lose authority and it
leaves your list. Revoking is not undone; the machine has to be enrolled again.

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.

Revoking a machine that is already disconnected reports success. If the device was lost or
compromised, rotate your ENV keys afterwards.

JSON output is supported with --json.

```
pb machine revoke <machine> [flags]
```

### Examples

```
  pb machine revoke old-laptop
  pb machine revoke old-laptop --confirm K3M9QX --json
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for revoke
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

