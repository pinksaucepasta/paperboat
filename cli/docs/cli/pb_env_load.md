## pb env load

Load your workspace ENV into the current shell

### Synopsis

Load the ENV recorded by `pb env hook --install` into the current shell, like sourcing a file. This works only through the pb shell function the hook defines in new shells; without the hook this command only says how to set it up. Scripts and CI can use `pb env run -- <command>` instead.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb env load [flags]
```

### Examples

```
  pb env hook --install   # once; then open a new shell
  pb env load
```

### Options

```
  -h, --help   help for load
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

