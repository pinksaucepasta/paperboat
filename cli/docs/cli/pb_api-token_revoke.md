## pb api-token revoke

Revoke an API token

### Synopsis

Revoke an API token by name or id. Requests that use it fail from then on.
You can revoke your own tokens; owners and admins can revoke any member's.
A name shared by several members' tokens must be given as an id. Without
--yes, the command asks for confirmation first.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb api-token revoke <name|id> [flags]
```

### Examples

```
  pb api-token revoke deploy
  pb api-token revoke apitok_123 --yes
```

### Options

```
  -h, --help   help for revoke
      --yes    revoke without asking
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

* [pb api-token](pb_api-token.md)	 - Create, list and revoke workspace API tokens

