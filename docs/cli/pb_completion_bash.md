## pb completion bash

Generate the autocompletion script for bash

### Synopsis

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

	source <(pb completion bash)

To load completions for every new session, execute once:

#### Linux:

	pb completion bash > /etc/bash_completion.d/pb

#### macOS:

	pb completion bash > $(brew --prefix)/etc/bash_completion.d/pb

You will need to start a new shell for this setup to take effect.


This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb completion bash
```

### Options

```
  -h, --help              help for bash
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

