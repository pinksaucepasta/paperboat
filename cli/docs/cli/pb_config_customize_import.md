## pb config customize import

Validate and replace local preferences from a JSON file

### Synopsis

Validate and replace local preferences from a JSON file

Validate the JSON preferences document, then replace local customization with its contents. A failed validation leaves the current preferences in place.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize import <file> [flags]
```

### Options

```
  -h, --help   help for import
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

