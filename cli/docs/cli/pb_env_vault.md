## pb env vault

Manage password-protected ENV key custody

### Synopsis

Manage password-protected ENV key custody

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for vault
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts
* [pb env vault init](pb_env_vault_init.md)	 - Create a password-protected ENV vault
* [pb env vault lock](pb_env_vault_lock.md)	 - Clear unlocked vault keys while retaining encrypted custody
* [pb env vault password](pb_env_vault_password.md)	 - Rewrap unlocked ENV keys with a new password
* [pb env vault recover](pb_env_vault_recover.md)	 - Recover vault access and replace the password and recovery code
* [pb env vault recovery](pb_env_vault_recovery.md)	 - Enable, replace, or disable the optional recovery code
* [pb env vault remove](pb_env_vault_remove.md)	 - Remove this device's local ENV vault custody
* [pb env vault reset](pb_env_vault_reset.md)	 - Replace personal ENV keys and delete every personal value
* [pb env vault resume](pb_env_vault_resume.md)	 - Reconcile an interrupted vault publication
* [pb env vault unlock](pb_env_vault_unlock.md)	 - Unlock this account's ENV vault on this device

