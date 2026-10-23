## pb team activity

Search the team audit log (owners, admins, auditors)

### Synopsis

Search the team audit log, newest first. Owners, admins and auditors can read it. Filter by actor (account ID, email or API token), action or action prefix, target, result (success or denied) and time range, or search text. Each event shows when, who, what, the result and the target. Use --json for the documented event shape and the next page cursor.

Events are kept for the plan's retention period (Team 365 days); export them with pb team activity export.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team activity <team> [flags]
```

### Options

```
      --action string   action, or a prefix such as team or billing
      --actor string    account ID, email or API token ID
      --cursor string   next_cursor from the previous page
  -h, --help            help for activity
      --json            print the events as JSON, with next_cursor
      --limit int       maximum events, 1–200 (default 50)
      --result string   success or denied
      --search string   text in the action, target name or actor email
      --since string    start: RFC 3339 time or a duration ago such as 24h or 7d
      --target string   target resource or machine ID
      --until string    end: RFC 3339 time or a duration ago
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions
* [pb team activity export](pb_team_activity_export.md)	 - Export the team audit log as CSV or JSON

