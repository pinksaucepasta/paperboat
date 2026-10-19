## pb login

Add this computer to your account, or sign in again, through browser approval

### Synopsis

Add this computer to your account, or sign in again, through browser approval

Alias for pb auth login. Open an approval link (printed with a code when this computer has no browser) on any device, sign in, choose the workspace and approve. A computer that is not on your account yet is added to the chosen workspace; one that already is only signs in again. Use --workspace to preselect the workspace. On a computer that is already added and signed in, login says where it stands; --fresh removes this computer's local setup and its machine record, then adds it again.

JSON output is supported with --json.

```
pb login [flags]
```

### Options

```
      --change-account   sign in with another account after browser approval
      --fresh            remove this computer's setup and machine record, then add it again
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

