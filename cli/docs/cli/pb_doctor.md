## pb doctor

Check Paperboat connectivity and readiness

### Synopsis

Check Paperboat connectivity and readiness

Check authentication, local runtime, and connectivity for the current setup or one selected machine. --repair performs supported local fixes; without it, the command reports findings and recovery guidance.

JSON output is supported with --json.

```
pb doctor [machine] [flags]
```

### Options

```
  -h, --help     help for doctor
      --json     print JSON
      --repair   repair Paperboat-owned local dependencies
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

