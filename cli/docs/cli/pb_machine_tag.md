## pb machine tag

Show or change a machine's tags

### Synopsis

Show a machine's tags, or change them in the active workspace's policy: add
or remove tags, or set the full list. Tags are tag:<name> (the tag: prefix may be
left out). A tag that does not exist yet is created; a tag left with no machines
is removed unless a rule uses it. A change that gives or takes anyone's access
(rules on the tag) is printed and must be confirmed; --yes confirms it. Members
may tag only machines they shared.

JSON output is supported with --json.

```
pb machine tag <machine> [add|remove|set] [tag...] [flags]
```

### Examples

```
  pb machine tag db
  pb machine tag db add prod ci
  pb machine tag db remove ci --yes
  pb machine tag db set prod
```

### Options

```
  -h, --help   help for tag
      --json   print JSON
      --yes    confirm the printed access changes without asking
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

