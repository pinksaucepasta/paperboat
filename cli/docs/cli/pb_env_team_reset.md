## pb env team reset

Discard all values and replace an encrypted ENV team key

### Synopsis

Discard all values and replace an encrypted ENV team key

Discard every value in the selected team scope and replace its key. Use the exact confirmation only when value loss is intended.

Team ENV scopes have their own encrypted keys and explicit account grants. Rotation preserves values while replacing access keys; reset discards values. Review the confirmation and affected members before changing custody.

JSON output is supported with --json.

```
pb env team reset <team> [flags]
```

### Options

```
      --confirm string   exact confirmation phrase: RESET ENV TEAM <team>
  -h, --help             help for reset
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes

