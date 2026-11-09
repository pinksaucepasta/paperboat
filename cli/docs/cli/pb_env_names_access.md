## pb env names access

Show or set whether commands on machines can list ENV names in this workspace

### Synopsis

Show, or with on|off set, "Commands on machines can list ENV names" for the active workspace (--workspace, default personal). It is on by default. Off, `pb env names` on the workspace's machines is refused; `pb env run` still gives commands their values, so this is a policy for the listing, not a lock. For a team only an owner or admin may change it.

JSON output is supported with --json.

```
pb env names access [on|off] [flags]
```

### Examples

```
  pb env names access
  pb env names access off
  pb env names access on --workspace my-team
```

### Options

```
  -h, --help   help for access
      --json   print the setting as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env names](pb_env_names.md)	 - List the names of the ENV variables commands on this machine receive (never values)

