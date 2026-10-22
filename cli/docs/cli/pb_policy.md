## pb policy

Read, test and change the workspace access policy

### Synopsis

The workspace policy is one file that says who may reach which machines,
previews and tunnels, and how. Read it with get, check a draft with test, see
what a change would do with set --dry-run, save it with set, explain any
decision with explain, and go back to an earlier version with restore. Every
save that changes anyone's access shows the changes and asks for confirmation.

With --json, this command group lists its available commands.

```
pb policy [flags]
```

### Options

```
  -h, --help   help for policy
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
* [pb policy explain](pb_policy_explain.md)	 - Explain who can do what under the policy
* [pb policy get](pb_policy_get.md)	 - Print the saved workspace policy
* [pb policy restore](pb_policy_restore.md)	 - Restore an earlier policy version
* [pb policy schema](pb_policy_schema.md)	 - Print the policy file JSON Schema
* [pb policy set](pb_policy_set.md)	 - Validate and save a workspace policy file
* [pb policy test](pb_policy_test.md)	 - Validate a policy file and run its tests
* [pb policy versions](pb_policy_versions.md)	 - List saved policy versions

