## pb recipe show

Show a recipe, its file and its machines

### Synopsis

Show one recipe: its revision, public token id, expiry and creator, the file as it
was created or last updated (comments kept), and every machine paired from it with
whether it is online, which revision it applied and any section that failed to
apply.

JSON output is supported with --json.

```
pb recipe show <name> [flags]
```

### Examples

```
  pb recipe show ci
  pb recipe show ci --json
```

### Options

```
  -h, --help   help for show
      --json   print JSON
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

