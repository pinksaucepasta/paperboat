## pb update approve-maintenance

Approve one exact supervisor release for a protected-workload interruption

### Synopsis

Approve one exact supervisor release for a protected-workload interruption

Approve one exact supervisor release for a protected workload interruption. --release binds the approval to that release instead of granting open-ended maintenance permission.

Updates use signed release metadata and preserve artifact authenticity and rollback protection. Check inspects an available release; status reports the installed updater state. A failed update should leave the known installation usable and report a recovery action.

JSON output is supported with --json.

```
pb update approve-maintenance [flags]
```

### Options

```
  -h, --help             help for approve-maintenance
      --json             print JSON
      --release string   exact signed release version to approve
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb update](pb_update.md)	 - Update pb from the signed Paperboat release

