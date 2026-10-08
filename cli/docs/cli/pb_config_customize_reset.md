## pb config customize reset

Reset only local CLI preferences; keep account and connection settings

### Synopsis

Reset only local CLI preferences; keep account and connection settings

Remove local shortcuts, command defaults, and appearance choices. This preserves sign-in credentials, configured server, and remote resource state; the confirmation code confirms the reset.

Customization is local to this CLI installation. It can change shortcuts, command defaults, and terminal appearance without changing account credentials or remote resources. Validate or explain preferences before applying an unfamiliar document.

JSON output is supported with --json.

```
pb config customize reset [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for reset
      --json             print machine-readable JSON
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

