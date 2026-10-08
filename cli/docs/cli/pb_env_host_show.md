## pb env host show

Show published ENV delivery and observed application on a host

### Synopsis

Show published ENV delivery and observed application on a host

Inspect the current published encrypted delivery separately from the host's observed application. Use --machine or this enrolled machine by default. JSON includes delivery metadata and observation only, without ciphertext or values; stale or absent observations do not mean applied.

ENV sync is automatic for authorized devices. Personal global values have device overrides. Team launches combine Team global values, the actor's member global values, and their device overrides. Values remain isolated by workspace.

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

* [pb env host](pb_env_host.md)	 - Inspect automatic workspace ENV delivery

