# W20: pb from an elevated shell, config directory owner and shared-lock parent (2026-11-11)

Runner: winpool-8 (hosted Windows Server 2025, 10.0.26100, UAC on, ConsentPromptBehaviorAdmin=0).
Builds: old = dev-32396cd1f (origin/main before the fix), new = dev-66b531865 (the fix; pushed as the same change on main).
Each case: a fresh local user `w20<case>` (member of Administrators), profile created by its first logon, deleted at the end.
- elevated: scheduled task as that user, RunLevel Highest (Administrators enabled, group owner; High).
- normal: the same user's task token reduced with `runas /trustlevel:0x20000` (Administrators deny-only), as a normal shell.
Each step runs `pb config set server https://api.example.test` (Config.Save), then `pb config local-access apply`
(Config.LockLocalAccess, then the install check; the runner has no Paperboat installed, so apply ends with rc=1 after the
lock, and the CLI prints a generic line for every cause), then prints owner + SDDL of the config dir and config.json.
Only Lock() changes an existing directory's owner (Save leaves existing directories alone), so the owner flipping
between "after config set" and the final line in the leftover cases shows the lock path ran its repair.

## Verdict
| Case | Result |
|---|---|
| old: elevated then normal | REPRO: config dir created owner BUILTIN\Administrators, inherited SY/BA/user; lock refuses it in both sessions (owner never changes) |
| new: elevated then normal | config dir created owner = user, protected D:PAI SY/BA/user; same in the normal session |
| new: normal then elevated | config dir owner = user, protected; elevated run keeps it |
| leftover (old elevated), then new normal, new elevated | normal (non-elevated) run repairs BA owner -> user, inherited -> protected DACL; elevated keeps it |
| leftover (old elevated), then new elevated, new normal | elevated run repairs BA owner -> user, protected DACL; normal keeps it |

Not covered here: a full `local-access apply` success needs an installed, paired Paperboat for that user.
Lock acceptance and refusal of foreign owners are covered by the workspace tests below (CI windows-latest, elevated phase).

## Workspace tests (CI run 42971802284, cli on windows-latest, -run . ./internal/config)
--- PASS: TestSharedLockCreatesProtectedOwnerAndLockMetadata (0.00s)
--- PASS: TestConfigDirectoryCreatedFromElevatedTokenIsOwnedByTheUser (0.01s)
--- PASS: TestSharedLockParentOwnedByAdministratorsIsRepairedToTheUser (0.01s)
--- PASS: TestSharedLockParentOwnedByAdministratorsIsRepairedToTheUser/protected (0.00s)
--- PASS: TestSharedLockParentOwnedByAdministratorsIsRepairedToTheUser/inherited (0.00s)
--- PASS: TestSharedLockParentWithAForeignOwnerIsRefusedUnchanged (0.00s)
--- PASS: TestSharedLockParentWithAForeignOwnerIsRefusedUnchanged/another_owner (0.00s)
--- PASS: TestSharedLockParentWithAForeignOwnerIsRefusedUnchanged/Administrators_with_an_extra_ACE (0.00s)
ok  	github.com/pinksaucepasta/paperboat/internal/config	4.197s
ok  	github.com/pinksaucepasta/paperboat/internal/config	0.087s

## Device output (runner hostname replaced by winrunner)

```
##### case old-EL: fresh user w20oldel (member of Administrators), steps: pb-old.exe:Highest:elevated,pb-old.exe:Limited:normal
=== elevated (pb-old.exe) ===
winrunner\w20oldel
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20oldel\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_1eb7845f-caba-4f06-9f64-dac6aea35761. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner BUILTIN\Administrators  C:\Users\w20oldel\AppData\Local\paperboat
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1005)
owner BUILTIN\Administrators  C:\Users\w20oldel\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1005)
C:\Users\w20oldel\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                                          BUILTIN\Administrators:(OI)(CI)(F)
                                          winrunner\w20oldel:(OI)(CI)(F)
### done
=== normal (pb-old.exe) ===
winrunner\w20oldel
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                   
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Group used for deny only                          
C:\Users\w20oldel\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_c6d95720-11b3-4a39-ab1c-37fc0c8bc686. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner BUILTIN\Administrators  C:\Users\w20oldel\AppData\Local\paperboat
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1005)
owner winrunner\w20oldel  C:\Users\w20oldel\AppData\Local\paperboat\config.json
  O:S-1-5-21-3289802278-1561746829-1498425070-1005G:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1005)
C:\Users\w20oldel\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                                          BUILTIN\Administrators:(OI)(CI)(F)
                                          winrunner\w20oldel:(OI)(CI)(F)
### done
##### case old-EL cleanup: user deleted; profiles left: 
```

```
##### case new-EL: fresh user w20newel (member of Administrators), steps: pb-new.exe:Highest:elevated,pb-new.exe:Limited:normal
=== elevated (pb-new.exe) ===
winrunner\w20newel
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20newel\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_f082a67d-70a9-4327-88b3-466aee278cac. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20newel  C:\Users\w20newel\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1006G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1006)
owner BUILTIN\Administrators  C:\Users\w20newel\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1006)
C:\Users\w20newel\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20newel:(F)
### done
=== normal (pb-new.exe) ===
winrunner\w20newel
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                   
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Group used for deny only                          
C:\Users\w20newel\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_f512fded-d038-45aa-b8b5-ac32e373a9c6. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20newel  C:\Users\w20newel\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1006G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1006)
owner winrunner\w20newel  C:\Users\w20newel\AppData\Local\paperboat\config.json
  O:S-1-5-21-3289802278-1561746829-1498425070-1006G:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1006)
C:\Users\w20newel\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20newel:(F)
### done
##### case new-EL cleanup: user deleted; profiles left: 
```

```
##### case new-LE: fresh user w20newle (member of Administrators), steps: pb-new.exe:Limited:normal,pb-new.exe:Highest:elevated
=== normal (pb-new.exe) ===
winrunner\w20newle
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                   
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Group used for deny only                          
C:\Users\w20newle\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_b740640a-44ef-4a02-a97c-5829a0f1709f. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20newle  C:\Users\w20newle\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1007G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1007)
owner winrunner\w20newle  C:\Users\w20newle\AppData\Local\paperboat\config.json
  O:S-1-5-21-3289802278-1561746829-1498425070-1007G:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1007)
C:\Users\w20newle\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20newle:(F)
### done
=== elevated (pb-new.exe) ===
winrunner\w20newle
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20newle\AppData\Local\paperboat\config.json
config-set rc=0
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_c8f82490-c7e3-49d6-a094-108ca2b8baff. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20newle  C:\Users\w20newle\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1007G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1007)
owner BUILTIN\Administrators  C:\Users\w20newle\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1007)
C:\Users\w20newle\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20newle:(F)
### done
##### case new-LE cleanup: user deleted; profiles left: 
```

```
##### case left-N: fresh user w20leftn (member of Administrators), steps: pb-old.exe:Highest:oldelevated,pb-new.exe:Limited:newnormal,pb-new.exe:Highest:newelevated
=== oldelevated (pb-old.exe) ===
winrunner\w20leftn
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20leftn\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner BUILTIN\Administrators
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_3ae8ff35-6c14-4b32-9fd9-e8f9059db1bb. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner BUILTIN\Administrators  C:\Users\w20leftn\AppData\Local\paperboat
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
owner BUILTIN\Administrators  C:\Users\w20leftn\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
C:\Users\w20leftn\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                                          BUILTIN\Administrators:(OI)(CI)(F)
                                          winrunner\w20leftn:(OI)(CI)(F)
### done
=== newnormal (pb-new.exe) ===
winrunner\w20leftn
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                   
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Group used for deny only                          
C:\Users\w20leftn\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner BUILTIN\Administrators
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_65d0b1bf-70a8-4604-a5c4-b5d81a4c42ef. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20leftn  C:\Users\w20leftn\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1008G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
owner winrunner\w20leftn  C:\Users\w20leftn\AppData\Local\paperboat\config.json
  O:S-1-5-21-3289802278-1561746829-1498425070-1008G:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
C:\Users\w20leftn\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20leftn:(F)
### done
=== newelevated (pb-new.exe) ===
winrunner\w20leftn
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20leftn\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner winrunner\w20leftn
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_f9ac51f2-d94d-4b8f-9794-58f792cf8a4f. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20leftn  C:\Users\w20leftn\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1008G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
owner BUILTIN\Administrators  C:\Users\w20leftn\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1008)
C:\Users\w20leftn\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20leftn:(F)
### done
##### case left-N cleanup: user deleted; profiles left: 
```

```
##### case left-E: fresh user w20lefte (member of Administrators), steps: pb-old.exe:Highest:oldelevated,pb-new.exe:Highest:newelevated,pb-new.exe:Limited:newnormal
=== oldelevated (pb-old.exe) ===
winrunner\w20lefte
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20lefte\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner BUILTIN\Administrators
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_75d38629-3e1c-4b26-ba84-0c2b873e92d4. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner BUILTIN\Administrators  C:\Users\w20lefte\AppData\Local\paperboat
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)(A;OICIID;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
owner BUILTIN\Administrators  C:\Users\w20lefte\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
C:\Users\w20lefte\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(OI)(CI)(F)
                                          BUILTIN\Administrators:(OI)(CI)(F)
                                          winrunner\w20lefte:(OI)(CI)(F)
### done
=== newelevated (pb-new.exe) ===
winrunner\w20lefte
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                                
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Mandatory group, Enabled by default, Enabled group, Group owner
C:\Users\w20lefte\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner BUILTIN\Administrators
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_119a5bc1-da95-4f24-88cf-039298cf42ca. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20lefte  C:\Users\w20lefte\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1009G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
owner BUILTIN\Administrators  C:\Users\w20lefte\AppData\Local\paperboat\config.json
  O:BAG:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
C:\Users\w20lefte\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20lefte:(F)
### done
=== newnormal (pb-new.exe) ===
winrunner\w20lefte
Mandatory Label\High Mandatory Level                          Label            S-1-16-12288                                                   
BUILTIN\Administrators                                        Alias            S-1-5-32-544 Group used for deny only                          
C:\Users\w20lefte\AppData\Local\paperboat\config.json
config-set rc=0
after config set: owner winrunner\w20lefte
pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_8ff316ca-308b-4e56-b8a7-f4dbb0ec1149. Run `pb bugreport --upload` to send the details.
local-access-apply rc=1
owner winrunner\w20lefte  C:\Users\w20lefte\AppData\Local\paperboat
  O:S-1-5-21-3289802278-1561746829-1498425070-1009G:S-1-5-21-3289802278-1561746829-1498425070-513D:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
owner winrunner\w20lefte  C:\Users\w20lefte\AppData\Local\paperboat\config.json
  O:S-1-5-21-3289802278-1561746829-1498425070-1009G:S-1-5-21-3289802278-1561746829-1498425070-513D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;S-1-5-21-3289802278-1561746829-1498425070-1009)
C:\Users\w20lefte\AppData\Local\paperboat NT AUTHORITY\SYSTEM:(F)
                                          BUILTIN\Administrators:(F)
                                          winrunner\w20lefte:(F)
### done
##### case left-E cleanup: user deleted; profiles left: 
```

## Cleanup
Test users deleted, scheduled tasks removed, C:\w20 removed, Paperboat never installed, no machine records created.
The deleted users' profile hives stay loaded by the system after the batch logons (reg unload: access denied); the
hosted runner VM is discarded at job end.
