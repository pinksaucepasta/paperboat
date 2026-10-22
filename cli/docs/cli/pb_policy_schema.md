## pb policy schema

Print the policy file JSON Schema

### Synopsis

Print the JSON Schema of the workspace policy file, for editors that check and
complete the file as you type. The schema is built into pb, so this command
works offline and needs no sign-in; it matches the schema the server serves.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb policy schema [flags]
```

### Examples

```
  pb policy schema > policy.schema.json
```

### Options

```
  -h, --help   help for schema
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

