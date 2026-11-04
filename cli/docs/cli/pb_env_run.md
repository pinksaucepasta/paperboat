## pb env run

Run a command with your workspace ENV in its environment

### Synopsis

Run a command with the ENV of the active workspace merged into its environment, for scripts and CI where no shell hook is installed. Values go straight to the child process: never to disk, logs or its arguments. Stdin, stdout, stderr, signals and the exit code pass through. If the ENV cannot be loaded, the command does not run.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb env run -- <command> [args...] [flags]
```

### Examples

```
  pb env run -- npm test
  pb env run --workspace my-team -- ./deploy.sh --prod
```

### Options

```
  -h, --help   help for run
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

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides

