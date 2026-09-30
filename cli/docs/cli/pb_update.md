## pb update

Update pb from the signed Paperboat release

### Synopsis

Update pb from the signed Paperboat release

Download and verify a signed Paperboat release, then review and approve its exact candidate before installation. Check and status are read-only; download never restarts services. Installation interrupts connections while services restart, with trusted recovery on failure.

JSON output is supported with --json.

```
pb update [flags]
```

### Options

```
      --approve string   install the exact downloaded candidate ID after reviewing it
  -h, --help             help for update
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb update check](pb_update_check.md)	 - Check the signed Paperboat release without installing it
* [pb update download](pb_update_download.md)	 - Download and verify an update without installing it
* [pb update status](pb_update_status.md)	 - Show installed Paperboat update state

