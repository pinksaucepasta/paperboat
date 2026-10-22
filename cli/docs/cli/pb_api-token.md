## pb api-token

Create, list and revoke workspace API tokens

### Synopsis

An API token lets a script or service call the Paperboat API as you, in one
workspace, until it expires or is revoked. A scope narrows what it may do using
the policy's names; a scope never gives more access than you have. The secret
is shown once, when the token is created.

With --json, this command group lists its available commands.

```
pb api-token [flags]
```

### Options

```
  -h, --help   help for api-token
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
* [pb api-token create](pb_api-token_create.md)	 - Create an API token and print its secret once
* [pb api-token list](pb_api-token_list.md)	 - List API tokens
* [pb api-token revoke](pb_api-token_revoke.md)	 - Revoke an API token

