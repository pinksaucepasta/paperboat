## pb api-token create

Create an API token and print its secret once

### Synopsis

Create an API token in the active workspace that acts as you until it
expires. --expires sets its lifetime in days (default 90d). --scope or
--scope-file narrows it to a JSON list of {"dst", "allow", "ports"} entries
using the policy's names. The secret is printed once; store it right away.

JSON output is supported with --json.

```
pb api-token create <name> [flags]
```

### Examples

```
  pb api-token create deploy
  pb api-token create ci --expires 30d --scope '[{"dst":["tag:ci"],"allow":["exec"]}]'
  pb api-token create ci --json
```

### Options

```
      --expires string      lifetime in days, such as 30d (default "90d")
  -h, --help                help for create
      --json                print JSON, including the secret
      --scope string        JSON list of {dst, allow, ports} entries that narrow the token
      --scope-file string   read the scope from a JSON file
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb api-token](pb_api-token.md)	 - Create, list and revoke workspace API tokens

