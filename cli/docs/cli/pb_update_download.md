## pb update download

Download and verify an update without installing it

### Synopsis

Download and verify an update without installing it

Download and verify the signed release without changing running services. Review the candidate and use pb update --approve with its exact ID to install. Approval selects the exact artifact. Installation preserves existing terminal sessions and established managed SSH connections and waits for finite work; --force permits interrupting work that cannot be preserved.

Updates use signed release metadata and preserve artifact authenticity and rollback protection. Check inspects an available release; status reports the installed updater state. A failed update should leave the known installation usable and report a recovery action.

JSON output is supported with --json.

```
pb update download [flags]
```

### Options

```
  -h, --help   help for download
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb update](pb_update.md)	 - Update pb from the signed Paperboat release

