## pb install

Install this executable and its local service

### Synopsis

Install this executable and its local service

Install this executable and its local background service on the current machine. --install-dir selects the binary location; use pb service status after installation to inspect the supervised daemon.

JSON output is supported with --json.

```
pb install [flags]
```

### Options

```
  -h, --help                 help for install
      --install-dir string   absolute directory for the Unix pb command
      --json                 print installation result as JSON
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

