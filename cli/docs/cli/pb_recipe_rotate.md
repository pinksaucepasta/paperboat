## pb recipe rotate

Give a recipe a new token; its machines keep working

### Synopsis

Give a recipe a new token. The old token stops working at once; machines already
paired from it keep running. Without --confirm, print what changes and the
confirmation code; run it again with --confirm <code>. The new token is printed once:
replace PB_RECIPE everywhere the old one is stored.

JSON output is supported with --json.

```
pb recipe rotate <name> [flags]
```

### Examples

```
  pb recipe rotate ci
  pb recipe rotate ci --confirm K3M9QX
```

### Options

```
      --confirm string   the confirmation code printed without --confirm
  -h, --help             help for rotate
      --json             print JSON (the new token included, once)
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

