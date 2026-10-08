## pb config customize explain

Show command expansion without executing it

### Synopsis

Show command expansion without executing it

Expand the supplied command arguments using the current local shortcuts and defaults without running the result. Place arguments after -- so they are interpreted as the proposed command.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize explain -- <arguments...> [flags]
```

### Options

```
  -h, --help   help for explain
      --json   print machine-readable JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config customize](pb_config_customize.md)	 - Customize local shortcuts, command defaults, and TUI appearance

