## pb env host show

Show published ENV delivery and observed application on a host

### Synopsis

Show published ENV delivery and observed application on a host

Inspect the current published encrypted delivery separately from the host's observed application. Use --machine or this enrolled machine by default. JSON includes delivery metadata and observation only, without ciphertext or values; stale or absent observations do not mean applied.

Host ENV projections contain an explicit selection of encrypted values for an enrolled machine. Provisioning changes the host selection; it does not expose values in command output or implicitly grant access to every scope.

JSON output is supported with --json.

```
pb env host show [flags]
```

### Options

```
  -h, --help             help for show
      --json             print host delivery metadata without ciphertext or keys
      --machine string   machine name or ID; defaults to this enrolled machine
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env host](pb_env_host.md)	 - Manage encrypted host ENV projections

