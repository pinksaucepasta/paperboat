## pb team activity export

Export the team audit log as CSV or JSON

### Synopsis

Download every audit log event that matches the filters, newest first, as a CSV file or a JSON array, for an archive or a SIEM. The same filters as pb team activity apply. The file is written to a temporary name and renamed when complete, so a failed download never leaves a partial file. Each export is itself recorded in the audit log.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team activity export <team> [flags]
```

### Options

```
      --action string   action, or a prefix such as team or billing
      --actor string    account ID, email or API token ID
      --format string   csv or json (default "csv")
  -h, --help            help for export
  -o, --output string   file to write, or - for standard output
      --result string   success or denied
      --search string   text in the action, target name or actor email
      --since string    start: RFC 3339 time or a duration ago such as 24h or 7d
      --target string   target resource or machine ID
      --until string    end: RFC 3339 time or a duration ago
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

* [pb team activity](pb_team_activity.md)	 - Search the team audit log (owners, admins, auditors)

