## pb recipe schema

Print the recipe file JSON Schema

### Synopsis

Print the JSON Schema of the recipe file, for editors and AI agents that write it.
The schema is built into pb, so this command works offline and needs no sign-in.
Point a file at it with a "$schema" key.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb recipe schema [flags]
```

### Examples

```
  pb recipe schema > recipe.schema.json
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

* [pb recipe](pb_recipe.md)	 - Create and manage recipes that pair and set up machines

