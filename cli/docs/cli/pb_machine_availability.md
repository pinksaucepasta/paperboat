## pb machine availability

Set machine sleep availability

### Synopsis

Choose whether a machine may sleep. --mode allow-sleep lets it sleep when idle; --mode
keep-awake keeps it available while idle, which can raise battery use and heat and keep a laptop
awake with the lid closed, so it needs a confirmation (--confirm, below).


The setting is saved first. The command then waits up to 5 seconds for the machine to report
that it applied it and says either "applied" or "saved; application is pending" (for example
while the machine is offline).

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.(Only keep-awake needs it.)

JSON output is supported with --json.

```
pb machine availability <machine> [flags]
```

### Examples

```
  pb machine availability db --mode keep-awake
  pb machine availability db --mode keep-awake --confirm K3M9QX
  pb machine availability db --mode allow-sleep
```

### Options

```
      --confirm string   six-character confirmation code for keep-awake mode
  -h, --help             help for availability
      --json             print JSON
      --mode string      availability mode: allow-sleep or keep-awake
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

