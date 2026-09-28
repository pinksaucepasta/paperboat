## pb env team rotate

Rotate an encrypted ENV team key while preserving its values

### Synopsis

Rotate an encrypted ENV team key while preserving its values

Replace the team scope key while preserving its values for remaining authorized members. The command requires an exact confirmation for this custody transition.

Team ENV scopes have their own encrypted keys and explicit account grants. Rotation preserves values while replacing access keys; reset discards values. Review the confirmation and affected members before changing custody.

JSON output is supported with --json.

```
pb env team rotate <team> [flags]
```

### Options

```
      --confirm string   exact confirmation phrase: ROTATE ENV TEAM <team>
  -h, --help             help for rotate
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes

