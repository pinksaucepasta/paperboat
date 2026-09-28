## pb update status

Show installed Paperboat update state

### Synopsis

Show installed Paperboat update state

Show the installed release and updater state, including any pending or failed transition. This reads local update state rather than downloading and installing a new release.

Updates use signed release metadata and preserve artifact authenticity and rollback protection. Check inspects an available release; status reports the installed updater state. A failed update should leave the known installation usable and report a recovery action.

JSON output is supported with --json.

```
pb update status [flags]
```

### Options

```
  -h, --help   help for status
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

