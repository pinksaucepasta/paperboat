## pb update

Update pb from the signed Paperboat release

### Synopsis

Update pb from the signed Paperboat release

Download and verify a signed Paperboat release, then review and approve its exact candidate before installation. Check and status are read-only; download never restarts services. Ordinary updates defer if replacing the process owner would interrupt protected work. --force explicitly permits interruption, while retaining signed verification and recovery. --from installs a local or HTTPS executable, package, or authenticated offline bundle. Bare supplied builds are custom; bundles retain official provenance.

JSON output is supported with --json.

```
pb update [flags]
```

### Options

```
      --approve string   install the exact downloaded candidate ID after reviewing it
      --force            install the signed update even if active sessions must be interrupted
      --from string      install an executable, package, or verified offline bundle from a local path or HTTPS URL
  -h, --help             help for update
      --json             print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb update bundle](pb_update_bundle.md)	 - Save an official update and its signed proof for offline installation
* [pb update check](pb_update_check.md)	 - Check the signed Paperboat release without installing it
* [pb update download](pb_update_download.md)	 - Download and verify an update without installing it
* [pb update settings](pb_update_settings.md)	 - View or change automatic update settings
* [pb update status](pb_update_status.md)	 - Show installed Paperboat update state

