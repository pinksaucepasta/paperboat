## pb config customize show

Show the local preference document

### Synopsis

Show the local preference document

Show the effective local preference document, including current shortcuts and appearance settings. The command does not execute any customized command.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize show [flags]
```

### Options

```
  -h, --help   help for show
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

