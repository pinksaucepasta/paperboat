## pb policy explain

Explain who can do what under the policy

### Synopsis

Explain an access decision. With --src, --dst and --action, print whether it
is allowed, the reason and the rules that decided it. With --dst and --action
only, list who can do it; with --src only, list what that user can do. --file
explains a draft instead of the saved policy and --api-token narrows the answer
to one of your API tokens. Members can explain their own access; owners and
admins anyone's.

JSON output is supported with --json.

```
pb policy explain [flags]
```

### Examples

```
  pb policy explain --src user:ana@example.test --dst machine:db --action private_access --port 5432
  pb policy explain --dst machine:db --action exec
  pb policy explain --src user:ana@example.test --file draft.hujson
```

### Options

```
      --action string      the action, such as exec or private_access
      --api-token string   narrow the answer to one of your API tokens, by name
      --dst string         what: machine:<alias>, preview:<name>, tunnel:<name>, tag:<tag>
      --file string        explain a draft policy file instead of the saved policy
  -h, --help               help for explain
      --json               print JSON
      --port int           the port, for private_access
      --src string         who: user:<email>
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb policy](pb_policy.md)	 - Read, test and change the workspace access policy

