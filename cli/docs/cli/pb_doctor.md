## pb doctor

Check Paperboat connectivity and readiness

### Synopsis

Check authentication, the local runtime and connectivity for this machine, or for one
machine you name. Without --repair the command only reports findings and how to recover; --repair
performs the supported local fixes (on Windows it includes a privileged step). Exit status is 0
when everything is healthy and 1 otherwise, so scripts should use --json and the exit status.

JSON output is supported with --json.

```
pb doctor [machine] [flags]
```

### Examples

```
  pb doctor
  pb doctor studio --json
  pb doctor --repair
```

### Options

```
  -h, --help     help for doctor
      --json     print JSON
      --repair   repair Paperboat-owned local dependencies (Windows) and restart the local daemon, then check
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

