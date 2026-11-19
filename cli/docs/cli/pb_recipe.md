## pb recipe

Create and manage recipes that pair and set up machines

### Synopsis

A recipe is a small JSON file (with comments, .jsonc) that says what happens on a machine when it
joins: its tags, what it may reach, which ENV vaults it reads, which repository it
pulls config from, which preview or tunnel it opens, and when it is removed. Create
one with pb recipe create -f ci.jsonc; the printed token, set as PB_RECIPE, pairs and
sets up a machine with one command and no prompt.

With --json, this command group lists its available commands.

```
pb recipe [flags]
```

### Options

```
  -h, --help   help for recipe
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

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb recipe create](pb_recipe_create.md)	 - Create a recipe from a file and print its token once
* [pb recipe list](pb_recipe_list.md)	 - List the workspace's recipes
* [pb recipe revoke](pb_recipe_revoke.md)	 - Revoke a recipe and remove its machines
* [pb recipe rotate](pb_recipe_rotate.md)	 - Give a recipe a new token; its machines keep working
* [pb recipe schema](pb_recipe_schema.md)	 - Print the recipe file JSON Schema
* [pb recipe show](pb_recipe_show.md)	 - Show a recipe, its file and its machines
* [pb recipe update](pb_recipe_update.md)	 - Change a recipe from its file

