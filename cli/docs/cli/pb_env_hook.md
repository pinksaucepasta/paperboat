## pb env hook

Load ENV in plain ssh and VS Code shells through a shell startup hook

### Synopsis

Without flags, print the hook line for a shell. --install adds that line to the shell's startup file (~/.bashrc, ~/.zshenv or ~/.config/fish/config.fish) for the account logged in here and the active workspace; --uninstall removes it. VS Code Remote-SSH: after installing, run "Remote-SSH: Kill VS Code Server on Host" and reconnect.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb env hook [bash|zsh|fish] [flags]
```

### Options

```
  -h, --help        help for hook
      --install     add the hook line to the shell startup file and record the account and workspace
      --status      show whether the hook is installed and whose ENV it loads
      --uninstall   remove the hook line from every shell startup file and forget the recorded choice
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

