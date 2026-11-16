## pb env

Manage automatic global ENV and device overrides

### Synopsis

Manage encrypted ENV values by personal, team or machine scope. Vault commands control key custody, while set and unset change exact values. Listing shows metadata without revealing a value.

How an AI agent finds and uses a secret:
  pb env names                       which ENV names commands here receive (never values)
  pb env run -- <command>            run a command with those values in its environment
  printf %s "$SECRET" | pb env set NAME --value-stdin     store a secret the user gives you
Never print, echo or write a value to a file or log: let the program that needs it read it from its environment.
`pb env run` gives a command the whole workspace set; there is no per-name scoping.

With --json, this command group lists its available commands.

```
pb env [flags]
```

### Options

```
  -h, --help   help for env
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
* [pb env grants](pb_env_grants.md)	 - Reconcile encrypted ENV team grants
* [pb env hook](pb_env_hook.md)	 - Load ENV in plain ssh and VS Code shells through a shell startup hook
* [pb env host](pb_env_host.md)	 - Inspect automatic workspace ENV delivery
* [pb env init](pb_env_init.md)	 - Create a password-protected ENV vault
* [pb env list](pb_env_list.md)	 - List configured environment-variable metadata
* [pb env load](pb_env_load.md)	 - Load your workspace ENV into the current shell
* [pb env lock](pb_env_lock.md)	 - Clear unlocked vault keys while retaining encrypted custody
* [pb env names](pb_env_names.md)	 - List the names of the ENV variables commands on this machine receive (never values)
* [pb env password](pb_env_password.md)	 - Rewrap unlocked ENV keys with a new password
* [pb env recover](pb_env_recover.md)	 - Recover vault access and replace the password and recovery code
* [pb env recovery](pb_env_recovery.md)	 - Enable, replace, or disable the optional recovery code
* [pb env remove](pb_env_remove.md)	 - Remove this machine's local ENV vault custody
* [pb env reset](pb_env_reset.md)	 - Replace personal ENV keys and delete every personal value
* [pb env resume](pb_env_resume.md)	 - Reconcile an interrupted vault publication
* [pb env rotate](pb_env_rotate.md)	 - Rotate personal ENV scope keys while preserving values
* [pb env run](pb_env_run.md)	 - Run a command with your workspace ENV in its environment
* [pb env set](pb_env_set.md)	 - Set one environment variable through a hidden prompt or bounded stdin
* [pb env team](pb_env_team.md)	 - Manage encrypted ENV team scopes
* [pb env unlock](pb_env_unlock.md)	 - Unlock this account's ENV vault on this machine
* [pb env unset](pb_env_unset.md)	 - Remove one environment variable
* [pb env vault](pb_env_vault.md)	 - Create, list and delete named ENV vaults that recipes read

