## pb policy test

Validate a policy file and run its tests

### Synopsis

Validate a policy file on the server without saving it, run the tests it
contains and print errors, test results and the access changes against the
saved policy. Without a file, the saved policy is tested. The command exits
with status 0 only when the policy is valid and every test passes.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb policy test [file] [flags]
```

### Examples

```
  pb policy test
  pb policy test policy.hujson
```

### Options

```
  -h, --help   help for test
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

* [pb policy](pb_policy.md)	 - Read, test and change the workspace access policy

