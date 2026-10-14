## pb update settings

View or change automatic update settings

### Synopsis

Show or change this machine's automatic update schedule.

Official installations enable scheduled updates by default. Custom builds require
a fresh official installation before they can use updates. Scheduled updates
download and verify releases ahead of the maintenance time, then install at that time
using the machine's local clock. The default maintenance time is 04:00.

Disabling automatic updates keeps availability checks enabled but leaves
downloads and installation to manual commands. Manual update check, download,
and install commands remain available either way. These settings apply to the
machine, independently of the selected account.

JSON output is supported with --json.

```
pb update settings [flags]
```

### Examples

```
  pb update settings
  pb update settings --auto=false
  pb update settings --auto=true --time 22:15
  pb update settings --time 04:30 --json
```

### Options

```
      --auto          enable or disable scheduled downloads and installs
  -h, --help          help for settings
      --json          print JSON
      --time string   set the machine-local daily update time (HH:MM)
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

