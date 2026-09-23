## pb completion fish

Generate the autocompletion script for fish

### Synopsis

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

	pb completion fish | source

To load completions for every new session, execute once:

	pb completion fish > ~/.config/fish/completions/pb.fish

You will need to start a new shell for this setup to take effect.


This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb completion fish [flags]
```

### Options

```
  -h, --help              help for fish
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb completion](pb_completion.md)	 - Generate the autocompletion script for the specified shell

