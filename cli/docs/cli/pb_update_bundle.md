## pb update bundle

Save an official update and its signed proof for offline installation

### Synopsis

Save an official update and its signed proof for offline installation

Save the official release for this platform together with its existing signed metadata to a new .pb-update file. Transfer that file to an offline machine and install with pb update --from. The receiver verifies signatures, expiration, artifact integrity and its retained rollback floors; no extra release asset is published. An interrupted signed update must finish its online recovery before offline installation.

Updates use signed release metadata and preserve artifact authenticity and rollback protection. Check inspects an available release; status reports the installed updater state. A failed update should leave the known installation usable and report a recovery action.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb update bundle --output <file.pb-update> [flags]
```

### Options

```
  -h, --help            help for bundle
      --output string   new .pb-update file for this platform
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb update](pb_update.md)	 - Update pb from the signed Paperboat release

