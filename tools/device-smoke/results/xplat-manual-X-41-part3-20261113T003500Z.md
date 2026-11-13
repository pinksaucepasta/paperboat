# X-41 part 3: Linux machine removed, seen from a fresh Windows client (FIX-WU), UTC 2026-11-13 00:00-00:40 (IST 05:30-06:10)

Builds: Windows `pb version` dev-aee62911e (main at the time) on winpool-4 (hosted runner, administrator accounts runneradmin, fixwu2, fixwu3), Linux `pb version` dev-aee62911e in a container on a test VM (user pbt). Fixed build = main + the change below.

## Cause
A second Windows administrator account on the host (profile created by Windows at its first sign-in) has `C:\Users\<name>` owned by BUILTIN\Administrators. `managedssh.verifyCurrentUserProfileRoot` accepted only the user or SYSTEM as owner (runneradmin's profile is SYSTEM-owned, which is why the first account worked), so `installWindowsOpenSSHConfig` failed with "Paperboat OpenSSH configuration conflicts with existing state" and the daemon never wrote `~/.ssh/config` / `paperboat_config`. Seen from outside: event `managed_ssh_failed` cause `internal`, then `managed_startup` degraded `ssh_target_not_ready`; `pb ssh`, `pb wait --for ssh`, `pb ssh doctor` fail. Win32-OpenSSH itself accepts an Administrators owner (the rule `openSecureSSHDirectory` already follows for `~/.ssh`).

## Reproduced (main, before the fix)
```
runneradmin (profile owner SYSTEM): pair -> .ssh gets config + paperboat_config; alias fixwu-l appears; after revoking fixwu-l on Linux the alias is gone within 5 s: PASS
fixwu3 (new admin account, profile owner Administrators): pair -> no .ssh folder at all; debug build wrote the daemon's real error "Paperboat OpenSSH configuration conflicts with existing state"
```
## After the fix (fixed build installed over it, service restarted)
```
fixwu3: .ssh has config-install marker, config, paperboat-managed-ssh.pub, paperboat_config
Linux machine fixwu-l2 paired: `alias lines: 2` in paperboat_config on W; `pb exec fixwu-l2 -- hostname` from W answers
Linux machine fixwu-l2 revoked on the Linux side: after 8 s `alias lines after removal: 0`; `pb exec fixwu-l2` says "No machine has that name or ID"
```
Test: TestValidWindowsProfileRootStateAllowsOnlyUserSystemOrAdministratorsDirectory (windows, PASS on the runner). Whole managedssh package on the runner: all pass except TestAgentServiceOwnsSocketAndManagedIdentityLifecycle ("managed SSH agent request denied", agent named-pipe test, not touched by this change; not investigated).

## Not covered here (row stays [~])
Every Mac pair, ENV delivery, config-sync entries, shared sessions, tunnels/previews seen from W, uninstall flavour, host-key trust wording after reset.
