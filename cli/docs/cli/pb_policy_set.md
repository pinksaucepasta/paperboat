## pb policy set

Validate and save a workspace policy file

### Synopsis

Check a policy file locally, validate it on the server against the saved
policy, print any errors, test results and the access each change grants or
removes, then save it. A save that changes anyone's access asks for
confirmation; --yes confirms the printed changes without asking. --dry-run
stops after validation. Use - to read the file from standard input.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb policy set <file|-> [flags]
```

### Examples

```
  pb policy set policy.hujson
  pb policy set policy.hujson --dry-run
  pb policy get | pb policy set - --yes --message "tidy"
```

### Options

```
      --dry-run          validate and print the changes without saving
  -h, --help             help for set
      --message string   describe the change in the version history
      --yes              confirm the printed access changes without asking
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

