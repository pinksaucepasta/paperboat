## pb preview ip-rules

Show or set which client addresses may open this preview

### Synopsis

Without flags, show the preview's IP allow and deny lists. --allow and --deny take an address or CIDR and repeat; each replaces that whole list. An address a deny entry matches is refused, and when the allow list is not empty only its addresses get in. Refused visitors see 403 "Access to this link is restricted".

JSON output is supported with --json.

```
pb preview ip-rules <preview> [flags]
```

### Options

```
      --allow stringArray   address or CIDR allowed in (repeatable; replaces the allow list)
      --clear               remove both lists
      --deny stringArray    address or CIDR refused (repeatable; replaces the deny list)
  -h, --help                help for ip-rules
      --json                print the lists as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

