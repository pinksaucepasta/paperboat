## pb recipe revoke

Revoke a recipe and remove its machines

### Synopsis

Revoke a recipe: its token stops working and every machine paired from it is
removed. Without --confirm, print what changes and the confirmation code; run it
again with --confirm <code>. Use pb recipe rotate instead to replace a leaked token
and keep the machines.

JSON output is supported with --json.

```
pb recipe revoke <name> [flags]
```

### Examples

```
  pb recipe revoke ci
  pb recipe revoke ci --confirm 7RT2WD
```

### Options

```
      --confirm string   the confirmation code printed without --confirm
  -h, --help             help for revoke
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

* [pb recipe](pb_recipe.md)	 - Create and manage recipes that pair and set up machines

