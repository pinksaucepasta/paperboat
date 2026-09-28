## pb env team revoke

Rotate an ENV team scope and revoke selected members

### Synopsis

Rotate an ENV team scope and revoke selected members

Remove selected members and rotate the team scope key so subsequent access is fenced. --total-loss chooses the destructive recovery path when preserving existing values is impossible.

Team ENV scopes have their own encrypted keys and explicit account grants. Rotation preserves values while replacing access keys; reset discards values. Review the confirmation and affected members before changing custody.

JSON output is supported with --json.

```
pb env team revoke <team> [flags]
```

### Options

```
      --confirm string       exact confirmation phrase for this destructive rotation
  -h, --help                 help for revoke
      --json                 print JSON
      --remove stringArray   account to remove; repeat for multiple accounts
      --total-loss           discard the previous team values while rotating the team key
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes

