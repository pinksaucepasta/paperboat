## pb update check

Check the signed Paperboat release without installing it

### Synopsis

Check the signed Paperboat release without installing it

Check trusted signed release metadata for a newer compatible artifact without installing it. The result can report availability and trust errors separately from installed state.

Updates use signed release metadata and preserve artifact authenticity and rollback protection. Check inspects an available release; status reports the installed updater state. A failed update should leave the known installation usable and report a recovery action.

JSON output is supported with --json.

```
pb update check [flags]
```

### Options

```
  -h, --help   help for check
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb update](pb_update.md)	 - Update pb from the signed Paperboat release

