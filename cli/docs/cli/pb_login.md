## pb login

Sign in through browser approval on any device

### Synopsis

Sign in through browser approval on any device

Alias for pb auth login. Print an approval link and sign in through WorkOS on any device, including when this machine has no browser. Credentials stay on the requesting CLI and installation or machine ownership is unchanged.

JSON output is supported with --json.

```
pb login [flags]
```

### Options

```
      --change-account   sign in with another account after browser approval
  -h, --help             help for login
      --json             print approval and sign-in states as JSON
      --no-browser       print the approval link without opening a browser
      --reauth           authenticate the current account again
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

