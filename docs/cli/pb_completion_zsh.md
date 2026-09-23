## pb completion zsh

Generate the autocompletion script for zsh

### Synopsis

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" >> ~/.zshrc

To load completions in your current shell session:

	source <(pb completion zsh)

To load completions for every new session, execute once:

#### Linux:

	pb completion zsh > "${fpath[1]}/_pb"

#### macOS:

	pb completion zsh > $(brew --prefix)/share/zsh/site-functions/_pb

You will need to start a new shell for this setup to take effect.


This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb completion zsh [flags]
```

### Options

```
  -h, --help              help for zsh
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

