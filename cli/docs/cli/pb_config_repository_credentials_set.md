## pb config repository credentials set

Read a credential profile from a private JSON file or stdin

### Synopsis

Read a credential profile from a private JSON file or stdin

Resolve a custom Git repository by ID or name and read its authentication profile from --file. Use --file - for stdin. The profile is validated and stored privately on this machine for repository access.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config repository credentials set <repository> [flags]
```

### Options

```
      --file string   private profile file; use - for stdin
  -h, --help          help for set
      --json          print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config repository credentials](pb_config_repository_credentials.md)	 - Store credentials privately on this machine

