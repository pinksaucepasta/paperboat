## pb config customize path

Print the local preference file path

### Synopsis

Print the local preference file path

Print the location of the local customization document. Use this path when editing or backing up preferences outside the CLI.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize path [flags]
```

### Options

```
  -h, --help   help for path
      --json   print machine-readable JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config customize](pb_config_customize.md)	 - Customize local shortcuts, command defaults, and TUI appearance

